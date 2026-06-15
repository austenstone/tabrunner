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
	"fmt"
	"strings"
	"syscall/js"

	"github.com/austenstone/tabrunner/internal/ghrunner"
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

// status reports a connect lifecycle stage to the page via the optional
// tabrunnerStatus(stage, detail) JS callback. Safe to call from any goroutine.
func status(stage, detail string) {
	cb := js.Global().Get("tabrunnerStatus")
	if cb.Type() == js.TypeFunction {
		cb.Invoke(stage, detail)
	}
}

// splitLabels turns "a, b ,c" into ["a","b","c"], dropping blanks.
func splitLabels(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// tabrunnerConnect(githubURL, regToken [, labels]) -> { error: string }
//
// Registers this tab as a real self-hosted runner and starts the long-poll
// listen loop in a goroutine. Returns immediately; progress is reported through
// the tabrunnerStatus JS callback. Broker traffic is routed through a CORS proxy
// whose origin defaults to http://localhost:8732/ but can be overridden by
// setting the tabrunnerProxyBase JS global (e.g. a deployed Cloudflare Worker).
func tabrunnerConnect(_ js.Value, args []js.Value) any {
	result := map[string]any{"error": ""}
	if len(args) < 2 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeString {
		result["error"] = "tabrunnerConnect: expected (githubURL, regToken[, labels])"
		return js.ValueOf(result)
	}

	githubURL := args[0].String()
	regToken := args[1].String()
	labels := "tabrunner"
	if len(args) > 2 && args[2].Type() == js.TypeString {
		if v := strings.TrimSpace(args[2].String()); v != "" {
			labels = v
		}
	}

	proxyBase := "http://localhost:8732/"
	if pb := js.Global().Get("tabrunnerProxyBase"); pb.Type() == js.TypeString {
		if v := strings.TrimSpace(pb.String()); v != "" {
			proxyBase = v
		}
	}
	// Route every outbound request through the CORS proxy. The broker and the
	// region-sharded pipelines host send no CORS headers, so direct fetch() from
	// the tab fails; the proxy re-emits ACAO:* for all of them.
	ghrunner.RequestURLRewrite = func(u string) string {
		if strings.HasPrefix(u, proxyBase) {
			return u
		}
		return proxyBase + u
	}

	// Step stdout/stderr always surfaces as clean [output] lines in #connect-log.
	ghrunner.OutputLog = func(line string) { status("output", line) }

	// Internal diagnostics are noisy; only wire them when ?debug=1 is present.
	search := js.Global().Get("location").Get("search").String()
	if strings.Contains(search, "debug=1") {
		ghrunner.DebugLog = func(s string) { status("debug", s) }
		runner.DebugLog = func(s string) { status("debug", s) }
	}

	go func() {
		ctx := context.Background()
		status("registering", "registering runner with GitHub")

		st, err := ghrunner.RegisterInMemory(ctx, ghrunner.RegisterOptions{
			GitHubURL: githubURL,
			RegToken:  regToken,
			Labels:    splitLabels(labels),
		})
		if err != nil {
			status("error", "register: "+err.Error())
			return
		}

		status("listening", "connected, waiting for jobs")
		handler := func(msgType, body string, msgID int64) error {
			status("job", fmt.Sprintf("message %d: %s", msgID, msgType))
			return nil
		}
		if err := ghrunner.ListenWithState(ctx, st, handler); err != nil {
			status("error", "listen: "+err.Error())
			return
		}
		status("disconnected", "listen loop ended")
	}()

	return js.ValueOf(result)
}

func main() {
	js.Global().Set("tabrunnerRun", js.FuncOf(tabrunnerRun))
	js.Global().Set("tabrunnerConnect", js.FuncOf(tabrunnerConnect))

	// Signal readiness so the page can flip from "loading" to "ready".
	if ready := js.Global().Get("tabrunnerReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	select {} // keep the Go runtime alive so the exported func stays callable
}
