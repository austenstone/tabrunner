package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/austenstone/tabrunner/internal/ghrunner"
	"github.com/austenstone/tabrunner/internal/runner"
	"github.com/austenstone/tabrunner/internal/workflow"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "connect":
		connectCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	job := fs.String("j", "", "run only the job with this id")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: tabrunner run <workflow.yml> [-j job]")
		os.Exit(2)
	}

	wf, err := workflow.Parse(fs.Arg(0))
	if err != nil {
		fatal("parse workflow: %v", err)
	}

	ctx := context.Background()
	r, err := runner.New(ctx, runner.DefaultContext())
	if err != nil {
		fatal("init runner: %v", err)
	}
	defer r.Close(ctx)

	if err := r.RunWorkflow(ctx, wf, *job); err != nil {
		fatal("%v", err)
	}
}

func connectCmd(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	url := fs.String("url", "", "GitHub org or repo URL (e.g. https://github.com/octodemo)")
	token := fs.String("token", "", "runner registration token")
	name := fs.String("name", "", "runner name (default: random tabrunner-xxxx)")
	handshakeOnly := fs.Bool("handshake-only", false, "only run the RemoteAuth handshake and print the result")
	tokenOnly := fs.Bool("token-only", false, "load existing runner state and fetch an OAuth access token, then exit")
	fs.Parse(args)

	ctx := context.Background()

	if *tokenOnly {
		if !ghrunner.StateExists("") {
			fatal("no runner state found; register first with: tabrunner connect --url <url> --token <reg-token>")
		}
		tok, exp, err := ghrunner.FetchAccessToken(ctx, "")
		if err != nil {
			fatal("%v", err)
		}
		prefix := tok
		if len(prefix) > 16 {
			prefix = prefix[:16] + "..."
		}
		fmt.Printf("access token ok ✅\n  token:   %s\n  len:     %d\n  expires: %s (in %s)\n",
			prefix, len(tok), exp.Format("15:04:05"), exp.Sub(time.Now()).Round(time.Second))
		return
	}

	if *handshakeOnly {
		if *url == "" || *token == "" {
			fmt.Fprintln(os.Stderr, "usage: tabrunner connect --url <github-url> --token <reg-token> --handshake-only")
			os.Exit(2)
		}
		res, err := ghrunner.Handshake(ctx, *url, *token)
		if err != nil {
			fatal("%v", err)
		}
		tokPrefix := res.Token
		if len(tokPrefix) > 12 {
			tokPrefix = tokPrefix[:12] + "..."
		}
		fmt.Printf("handshake ok\n  service url:  %s\n  token schema: %s\n  token:        %s\n  use_v2_flow:  %v\n",
			res.URL, res.TokenSchema, tokPrefix, res.UseV2Flow)
		return
	}

	if ghrunner.StateExists("") {
		s, err := ghrunner.LoadSettings("")
		if err != nil {
			fatal("load existing runner state: %v", err)
		}
		fmt.Printf("already registered as %s (id %d). Delete ./.tabrunner to re-register.\n", s.AgentName, s.AgentID)
		return
	}

	if *url == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: tabrunner connect --url <github-url> --token <reg-token>")
		os.Exit(2)
	}

	s, err := ghrunner.Register(ctx, ghrunner.RegisterOptions{
		GitHubURL: *url,
		RegToken:  *token,
		Name:      *name,
	})
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("registered as a self-hosted runner ✅\n  name:    %s\n  id:      %d\n  pool:    %d\n  server:  %s\ncredentials saved to ./.tabrunner\n",
		s.AgentName, s.AgentID, s.PoolID, s.ServerURL)
}

func usage() {
	fmt.Println(`tabrunner - the first WebAssembly GitHub Actions runner

Usage:
  tabrunner run <workflow.yml> [-j job]            Execute a workflow locally in Wasm
  tabrunner connect --url <url> --token <token>    Register as a GitHub self-hosted runner

Each run: step executes inside a wazero sandbox via the embedded wasmsh shell.`)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[31merror:\033[0m "+format+"\n", a...)
	os.Exit(1)
}
