package main

import (
	"context"
	"flag"
	"fmt"
	"os"

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

func usage() {
	fmt.Println(`tabrunner - the first WebAssembly GitHub Actions runner

Usage:
  tabrunner run <workflow.yml> [-j job]   Execute a workflow locally in Wasm

Each run: step executes inside a wazero sandbox via the embedded wasmsh shell.`)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[31merror:\033[0m "+format+"\n", a...)
	os.Exit(1)
}
