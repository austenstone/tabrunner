// Package runner orchestrates execution of a parsed workflow. For M0 it runs
// each `run:` step inside a Wasm sandbox (wazero) using the embedded wasmsh
// shell, and wires up the GitHub Actions file-command protocol so that outputs
// and env vars flow between steps.
//
// Filesystem state lives in an in-memory FS (internal/memfs) shared between the
// host and the Wasm guest. That keeps the runner free of any real-disk
// dependency, so the exact same code runs natively and in the browser
// (GOOS=js), where there is no usable OS filesystem.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/austenstone/tabrunner/internal/memfs"
	"github.com/austenstone/tabrunner/internal/wasmsh"
	"github.com/austenstone/tabrunner/internal/workflow"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// Context carries repo/run metadata used to populate GITHUB_* / RUNNER_* vars
// and resolve `${{ github.* }}` / `${{ runner.* }}` expressions.
type Context struct {
	Repository string
	SHA        string
	Ref        string
	RunID      string
	RunnerOS   string
	RunnerArch string
}

// DefaultContext returns a sensible local-dev context.
func DefaultContext() Context {
	return Context{
		Repository: "local/tabrunner",
		SHA:        "0000000000000000000000000000000000000000",
		Ref:        "refs/heads/main",
		RunID:      "1",
		RunnerOS:   "Wasm",
		RunnerArch: "wasm32",
	}
}

// Runner executes workflows.
type Runner struct {
	ctx      Context
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	out      io.Writer
}

// New creates a Runner with a compiled wasmsh module ready to instantiate.
func New(ctx context.Context, rc Context) (*Runner, error) {
	rt := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	compiled, err := rt.CompileModule(ctx, wasmsh.Bytes())
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compile wasmsh: %w", err)
	}
	return &Runner{ctx: rc, rt: rt, compiled: compiled, out: os.Stdout}, nil
}

// SetOutput redirects all runner and step output to w (defaults to os.Stdout).
// The browser build uses this to stream logs into the page.
func (r *Runner) SetOutput(w io.Writer) {
	if w != nil {
		r.out = w
	}
}

// Close releases the wazero runtime.
func (r *Runner) Close(ctx context.Context) error {
	return r.rt.Close(ctx)
}

// RunWorkflow executes every job (or just one if jobFilter is non-empty).
func (r *Runner) RunWorkflow(ctx context.Context, wf *workflow.Workflow, jobFilter string) error {
	for _, job := range wf.Jobs {
		if jobFilter != "" && job.ID != jobFilter {
			continue
		}
		fmt.Fprintf(r.out, "\n\033[1m== Job: %s ==\033[0m\n", job.Name)
		if err := r.runJob(ctx, wf, job); err != nil {
			return fmt.Errorf("job %q failed: %w", job.ID, err)
		}
	}
	return nil
}

// Guest paths (inside the Wasm sandbox, where the in-memory FS maps to "/").
// The host reads/writes the same paths through the shared memfs.
const (
	guestOutput = "/github/output"
	guestEnv    = "/github/env"
	guestPath   = "/github/path"
	guestState  = "/github/state"
	guestWork   = "/work"
)

func (r *Runner) runJob(ctx context.Context, wf *workflow.Workflow, job *workflow.Job) error {
	fsys := memfs.New()
	for _, d := range []string{guestWork, "/github"} {
		if err := fsys.MkdirAll(d); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	jobEnv := r.baseEnv()
	jobEnv["GITHUB_WORKSPACE"] = guestWork
	jobEnv["TABRUNNER_CWD"] = guestWork
	mergeEnv(jobEnv, wf.Env)
	mergeEnv(jobEnv, job.Env)

	stepOutputs := map[string]map[string]string{}
	var pathPrepends []string

	for i, step := range job.Steps {
		fmt.Fprintf(r.out, "\n\033[36m-> %s\033[0m\n", step.DisplayName())

		if step.Uses != "" {
			fmt.Fprintf(r.out, "   \033[33m(skipped: `uses:` actions are not supported in M0)\033[0m\n")
			continue
		}
		if strings.TrimSpace(step.Run) == "" {
			continue
		}

		// Fresh, empty file-command files before each step.
		for _, p := range []string{guestOutput, guestEnv, guestPath, guestState} {
			if err := fsys.WriteFile(p, nil); err != nil {
				return err
			}
		}

		stepEnv := cloneEnv(jobEnv)
		mergeEnv(stepEnv, step.Env)
		stepEnv["GITHUB_OUTPUT"] = guestOutput
		stepEnv["GITHUB_ENV"] = guestEnv
		stepEnv["GITHUB_PATH"] = guestPath
		stepEnv["GITHUB_STATE"] = guestState
		if len(pathPrepends) > 0 {
			stepEnv["PATH"] = strings.Join(pathPrepends, ":") + ":" + stepEnv["PATH"]
		}

		script := r.expandExpressions(step.Run, stepEnv, stepOutputs)

		exit, err := r.runStep(ctx, fsys, script, stepEnv)
		if err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}

		// Parse file-command outputs regardless of exit code.
		outs := parseKeyVals(fsys, guestOutput)
		if step.ID != "" && len(outs) > 0 {
			stepOutputs[step.ID] = outs
		}
		for k, v := range parseKeyVals(fsys, guestEnv) {
			jobEnv[k] = v
		}
		if extra := parsePathFile(fsys, guestPath); len(extra) > 0 {
			pathPrepends = append(extra, pathPrepends...)
		}

		if exit != 0 {
			if step.ContinueOnError {
				fmt.Fprintf(r.out, "   \033[33m(exit %d, continue-on-error)\033[0m\n", exit)
				continue
			}
			return fmt.Errorf("step %q exited with code %d", step.DisplayName(), exit)
		}
	}

	fmt.Fprintf(r.out, "\n\033[32m== Job %s completed ==\033[0m\n", job.Name)
	return nil
}

// runStep executes a single script inside the Wasm sandbox. Returns the exit code.
func (r *Runner) runStep(ctx context.Context, fsys *memfs.FS, script string, env map[string]string) (int, error) {
	fsConfig := wazero.NewFSConfig().(sysfs.FSConfig).WithSysFSMount(fsys, "/")

	cfg := wazero.NewModuleConfig().
		WithStdin(strings.NewReader(script)).
		WithStdout(r.out).
		WithStderr(r.out).
		WithFSConfig(fsConfig).
		WithArgs("wasmsh").
		WithName("") // anonymous so we can instantiate repeatedly

	for k, v := range env {
		cfg = cfg.WithEnv(k, v)
	}

	_, err := r.rt.InstantiateModule(ctx, r.compiled, cfg)
	if err == nil {
		return 0, nil
	}

	var exitErr *sys.ExitError
	if errors.As(err, &exitErr) {
		return int(exitErr.ExitCode()), nil
	}
	return 1, err
}

// baseEnv returns the default GITHUB_*/RUNNER_* environment for a job.
func (r *Runner) baseEnv() map[string]string {
	return map[string]string{
		"CI":                "true",
		"GITHUB_ACTIONS":    "true",
		"GITHUB_REPOSITORY": r.ctx.Repository,
		"GITHUB_SHA":        r.ctx.SHA,
		"GITHUB_REF":        r.ctx.Ref,
		"GITHUB_RUN_ID":     r.ctx.RunID,
		"RUNNER_OS":         r.ctx.RunnerOS,
		"RUNNER_ARCH":       r.ctx.RunnerArch,
		"PATH":              "/usr/local/bin:/usr/bin:/bin",
		"HOME":              "/work",
	}
}

// --- ${{ }} expression expansion (minimal, host-side) ----------------------

var exprRe = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

func (r *Runner) expandExpressions(s string, env map[string]string, stepOutputs map[string]map[string]string) string {
	return exprRe.ReplaceAllStringFunc(s, func(match string) string {
		inner := exprRe.FindStringSubmatch(match)[1]
		return r.resolveExpr(inner, env, stepOutputs)
	})
}

func (r *Runner) resolveExpr(expr string, env map[string]string, stepOutputs map[string]map[string]string) string {
	expr = strings.TrimSpace(expr)

	// Quoted literal.
	if len(expr) >= 2 && (expr[0] == '\'' || expr[0] == '"') && expr[len(expr)-1] == expr[0] {
		return expr[1 : len(expr)-1]
	}

	switch {
	case strings.HasPrefix(expr, "steps."):
		// steps.<id>.outputs.<name>
		parts := strings.Split(expr, ".")
		if len(parts) == 4 && parts[2] == "outputs" {
			if outs, ok := stepOutputs[parts[1]]; ok {
				return outs[parts[3]]
			}
		}
		return ""
	case strings.HasPrefix(expr, "env."):
		return env[strings.TrimPrefix(expr, "env.")]
	case strings.HasPrefix(expr, "github."):
		switch strings.TrimPrefix(expr, "github.") {
		case "repository":
			return r.ctx.Repository
		case "sha":
			return r.ctx.SHA
		case "ref":
			return r.ctx.Ref
		case "run_id":
			return r.ctx.RunID
		}
		return ""
	case strings.HasPrefix(expr, "runner."):
		switch strings.TrimPrefix(expr, "runner.") {
		case "os":
			return r.ctx.RunnerOS
		case "arch":
			return r.ctx.RunnerArch
		}
		return ""
	}
	return ""
}

// --- file-command parsing --------------------------------------------------

// parseKeyVals reads a GITHUB_OUTPUT / GITHUB_ENV file supporting both the
// `key=value` form and the heredoc form:
//
//	key<<DELIM
//	multi
//	line
//	DELIM
func parseKeyVals(fsys *memfs.FS, path string) map[string]string {
	data, ok := fsys.ReadFile(path)
	if !ok {
		return nil
	}
	result := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		if key, delim, ok := parseHeredocStart(line); ok {
			var body []string
			i++
			for i < len(lines) && lines[i] != delim {
				body = append(body, lines[i])
				i++
			}
			result[key] = strings.Join(body, "\n")
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			result[k] = v
		}
	}
	return result
}

func parseHeredocStart(line string) (key, delim string, ok bool) {
	idx := strings.Index(line, "<<")
	if idx == -1 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	delim = strings.TrimSpace(line[idx+2:])
	if key == "" || delim == "" {
		return "", "", false
	}
	return key, delim, true
}

func parsePathFile(fsys *memfs.FS, path string) []string {
	data, ok := fsys.ReadFile(path)
	if !ok {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// --- env helpers -----------------------------------------------------------

func cloneEnv(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func mergeEnv(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}
