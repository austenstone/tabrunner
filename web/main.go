//go:build js && wasm

// Command tabrunner-web is the browser entry point. It compiles to
// GOOS=js GOARCH=wasm and exposes a single global function, tabrunnerRun, that
// the page calls to execute a workflow entirely in the tab.
//
// wazero auto-selects its pure-Go interpreter under js/wasm, so the same runner
// code that works natively runs here too, with the in-memory FS standing in for
// a real disk.
package main

import (
	"bytes"
	"context"
	"syscall/js"

	"github.com/austenstone/tabrunner/internal/runner"
	"github.com/austenstone/tabrunner/internal/workflow"
)

// tabrunnerRun(yamlSource [, jobFilter]) -> { output: string, error: string }
//
// It never throws: parse and run errors are returned in the result object so
// the UI can render them alongside whatever output was produced.
func tabrunnerRun(_ js.Value, args []js.Value) any {
	result := map[string]any{"output": "", "error": ""}
	if len(args) < 1 || args[0].Type() != js.TypeString {
		result["error"] = "tabrunnerRun: expected a workflow YAML string"
		return js.ValueOf(result)
	}

	src := args[0].String()
	jobFilter := ""
	if len(args) > 1 && args[1].Type() == js.TypeString {
		jobFilter = args[1].String()
	}

	wf, err := workflow.ParseBytes([]byte(src))
	if err != nil {
		result["error"] = "parse workflow: " + err.Error()
		return js.ValueOf(result)
	}

	var buf bytes.Buffer
	ctx := context.Background()

	r, err := runner.New(ctx, runner.DefaultContext())
	if err != nil {
		result["error"] = "init runner: " + err.Error()
		return js.ValueOf(result)
	}
	defer r.Close(ctx)
	r.SetOutput(&buf)

	if runErr := r.RunWorkflow(ctx, wf, jobFilter); runErr != nil {
		result["error"] = runErr.Error()
	}
	result["output"] = buf.String()
	return js.ValueOf(result)
}

func main() {
	js.Global().Set("tabrunnerRun", js.FuncOf(tabrunnerRun))

	// Signal readiness so the page can flip from "loading" to "ready".
	if ready := js.Global().Get("tabrunnerReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	select {} // keep the Go runtime alive so the exported func stays callable
}
