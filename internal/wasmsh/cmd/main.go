// Command wasmsh is a minimal POSIX-ish shell compiled to wasm32-wasip1 and
// embedded into tabrunner. It is NOT a real bash. It exists so that a workflow
// `run:` step can execute *inside* the Wasm sandbox while still supporting the
// pieces GitHub Actions relies on: variable expansion, a handful of coreutils
// builtins, and `>>`/`>` redirection (so `echo "k=v" >> $GITHUB_OUTPUT` works).
//
// The script is read from stdin. Environment comes from the host. If the env
// var TABRUNNER_CWD is set, the shell chdirs there before running.
//
// `set -e` behaviour is ON by default, matching how Actions invokes bash.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type shell struct {
	errexit bool
	out     io.Writer
	errOut  io.Writer
}

func main() {
	sh := &shell{errexit: true, out: os.Stdout, errOut: os.Stderr}

	if cwd := os.Getenv("TABRUNNER_CWD"); cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			fmt.Fprintf(sh.errOut, "wasmsh: cd %s: %v\n", cwd, err)
			os.Exit(1)
		}
	}

	script, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		fmt.Fprintf(sh.errOut, "wasmsh: read script: %v\n", err)
		os.Exit(1)
	}

	os.Exit(sh.runScript(string(script)))
}

// runScript executes each logical line, honouring `&&`, `;` and `set -e`.
func (sh *shell) runScript(script string) int {
	for _, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Split on `&&`: stop the chain on first failure.
		code := 0
		for _, seg := range splitTop(line, "&&") {
			for _, stmt := range splitTop(seg, ";") {
				stmt = strings.TrimSpace(stmt)
				if stmt == "" {
					continue
				}
				code = sh.runStatement(stmt)
				if code != 0 {
					break
				}
			}
			if code != 0 {
				break
			}
		}
		if code != 0 && sh.errexit {
			return code
		}
	}
	return 0
}

// runStatement tokenizes and runs a single command, handling redirection.
func (sh *shell) runStatement(stmt string) int {
	tokens, err := tokenize(stmt)
	if err != nil {
		fmt.Fprintf(sh.errOut, "wasmsh: %v\n", err)
		return 1
	}
	if len(tokens) == 0 {
		return 0
	}

	// Pull off redirection: `>> file` or `> file`.
	var redirectPath string
	var appendMode bool
	clean := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		switch tokens[i] {
		case ">>", ">":
			appendMode = tokens[i] == ">>"
			if i+1 >= len(tokens) {
				fmt.Fprintln(sh.errOut, "wasmsh: syntax error near redirection")
				return 1
			}
			redirectPath = tokens[i+1]
			i++
		default:
			clean = append(clean, tokens[i])
		}
	}
	if len(clean) == 0 {
		return 0
	}

	out := sh.out
	if redirectPath != "" {
		flags := os.O_CREATE | os.O_WRONLY
		if appendMode {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(redirectPath, flags, 0o644)
		if err != nil {
			fmt.Fprintf(sh.errOut, "wasmsh: %s: %v\n", redirectPath, err)
			return 1
		}
		defer f.Close()
		out = f
	}

	return sh.dispatch(clean, out)
}

// dispatch runs a builtin. Unknown commands are an error (exit 127).
func (sh *shell) dispatch(args []string, out io.Writer) int {
	cmd, rest := args[0], args[1:]
	switch cmd {
	case ":", "true":
		return 0
	case "false":
		return 1
	case "echo":
		return builtinEcho(out, rest)
	case "printf":
		return builtinPrintf(out, sh.errOut, rest)
	case "cat":
		return builtinCat(out, sh.errOut, rest)
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Fprintln(out, wd)
		return 0
	case "cd":
		return builtinCd(sh.errOut, rest)
	case "export":
		return builtinExport(rest)
	case "env":
		return builtinEnv(out)
	case "set":
		return sh.builtinSet(rest)
	case "mkdir":
		return builtinMkdir(sh.errOut, rest)
	case "touch":
		return builtinTouch(sh.errOut, rest)
	case "ls":
		return builtinLs(out, sh.errOut, rest)
	case "rm":
		return builtinRm(sh.errOut, rest)
	default:
		fmt.Fprintf(sh.errOut, "wasmsh: %s: command not found\n", cmd)
		return 127
	}
}

func builtinEcho(out io.Writer, args []string) int {
	newline := true
	if len(args) > 0 && args[0] == "-n" {
		newline = false
		args = args[1:]
	}
	fmt.Fprint(out, strings.Join(args, " "))
	if newline {
		fmt.Fprintln(out)
	}
	return 0
}

func builtinPrintf(out, errOut io.Writer, args []string) int {
	if len(args) == 0 {
		return 0
	}
	format := args[0]
	rest := make([]any, len(args)-1)
	for i, a := range args[1:] {
		rest[i] = a
	}
	// Support the common escapes; Go's Fprintf handles the verbs.
	format = strings.ReplaceAll(format, "\\n", "\n")
	format = strings.ReplaceAll(format, "\\t", "\t")
	fmt.Fprintf(out, format, rest...)
	return 0
}

func builtinCat(out, errOut io.Writer, args []string) int {
	if len(args) == 0 {
		data, _ := io.ReadAll(os.Stdin)
		out.Write(data)
		return 0
	}
	code := 0
	for _, name := range args {
		data, err := os.ReadFile(name)
		if err != nil {
			fmt.Fprintf(errOut, "cat: %s: %v\n", name, err)
			code = 1
			continue
		}
		out.Write(data)
	}
	return code
}

func builtinCd(errOut io.Writer, args []string) int {
	target := os.Getenv("HOME")
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" {
		target = "/"
	}
	if err := os.Chdir(target); err != nil {
		fmt.Fprintf(errOut, "cd: %s: %v\n", target, err)
		return 1
	}
	return 0
}

func builtinExport(args []string) int {
	for _, a := range args {
		if k, v, ok := strings.Cut(a, "="); ok {
			os.Setenv(k, v)
		}
	}
	return 0
}

func builtinEnv(out io.Writer) int {
	env := os.Environ()
	sort.Strings(env)
	for _, e := range env {
		fmt.Fprintln(out, e)
	}
	return 0
}

func (sh *shell) builtinSet(args []string) int {
	for _, a := range args {
		switch a {
		case "-e":
			sh.errexit = true
		case "+e":
			sh.errexit = false
		}
	}
	return 0
}

func builtinMkdir(errOut io.Writer, args []string) int {
	parents := false
	paths := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-p" {
			parents = true
			continue
		}
		paths = append(paths, a)
	}
	code := 0
	for _, p := range paths {
		var err error
		if parents {
			err = os.MkdirAll(p, 0o755)
		} else {
			err = os.Mkdir(p, 0o755)
		}
		if err != nil {
			fmt.Fprintf(errOut, "mkdir: %s: %v\n", p, err)
			code = 1
		}
	}
	return code
}

func builtinTouch(errOut io.Writer, args []string) int {
	code := 0
	for _, name := range args {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(errOut, "touch: %s: %v\n", name, err)
			code = 1
			continue
		}
		f.Close()
	}
	return code
}

func builtinLs(out, errOut io.Writer, args []string) int {
	dir := "."
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			dir = a
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(errOut, "ls: %s: %v\n", dir, err)
		return 1
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(out, n)
	}
	return 0
}

func builtinRm(errOut io.Writer, args []string) int {
	recursive := false
	force := false
	paths := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a == "-r" || a == "-rf" || a == "-fr" || a == "-R":
			recursive = true
			if a != "-r" && a != "-R" {
				force = true
			}
		case a == "-f":
			force = true
		case strings.HasPrefix(a, "-"):
			// ignore unknown flags
		default:
			paths = append(paths, a)
		}
	}
	code := 0
	for _, p := range paths {
		var err error
		if recursive {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		if err != nil && !force {
			fmt.Fprintf(errOut, "rm: %s: %v\n", p, err)
			code = 1
		}
	}
	return code
}

// --- tokenizer -------------------------------------------------------------

// tokenize splits a statement into words, honouring single/double quotes and
// performing $VAR / ${VAR} expansion outside of single quotes. Redirection
// operators `>>` and `>` are emitted as standalone tokens.
func tokenize(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	hasCur := false
	i := 0

	flush := func() {
		if hasCur {
			tokens = append(tokens, cur.String())
			cur.Reset()
			hasCur = false
		}
	}

	for i < len(s) {
		c := s[i]
		switch c {
		case ' ', '\t':
			flush()
			i++
		case '>':
			flush()
			if i+1 < len(s) && s[i+1] == '>' {
				tokens = append(tokens, ">>")
				i += 2
			} else {
				tokens = append(tokens, ">")
				i++
			}
		case '\'':
			hasCur = true
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i = j + 1
		case '"':
			hasCur = true
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					switch s[j+1] {
					case '$', '`', '"', '\\', '\n':
						cur.WriteByte(s[j+1])
						j += 2
						continue
					}
					cur.WriteByte(s[j])
					j++
					continue
				}
				if s[j] == '$' {
					name, adv := readVar(s[j:])
					cur.WriteString(os.Getenv(name))
					j += adv
					continue
				}
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i = j + 1
		case '$':
			hasCur = true
			name, adv := readVar(s[i:])
			cur.WriteString(os.Getenv(name))
			i += adv
		case '\\':
			hasCur = true
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		default:
			hasCur = true
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return tokens, nil
}

// readVar reads a $NAME or ${NAME} starting at s[0]=='$' and returns the var
// name plus how many bytes to advance.
func readVar(s string) (name string, advance int) {
	if len(s) < 2 {
		return "", 1
	}
	if s[1] == '{' {
		end := strings.IndexByte(s, '}')
		if end == -1 {
			return "", len(s)
		}
		return s[2:end], end + 1
	}
	j := 1
	for j < len(s) && (isAlnum(s[j]) || s[j] == '_') {
		j++
	}
	return s[1:j], j
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// splitTop splits s on sep but ignores sep occurrences inside quotes.
func splitTop(s, sep string) []string {
	var parts []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' && !inDouble {
			inSingle = !inSingle
		} else if c == '"' && !inSingle {
			inDouble = !inDouble
		}
		if !inSingle && !inDouble && strings.HasPrefix(s[i:], sep) {
			parts = append(parts, cur.String())
			cur.Reset()
			i += len(sep) - 1
			continue
		}
		cur.WriteByte(c)
	}
	parts = append(parts, cur.String())
	return parts
}
