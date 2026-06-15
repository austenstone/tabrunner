package ghrunner

import (
	"fmt"
	"strings"
)

// DebugLog, when set, receives human-readable diagnostic lines from the job
// execution path. The js/wasm entrypoint wires this to the UI status channel so
// failures surface reliably in the browser (stdout/console capture is fragile).
var DebugLog func(string)

// OutputLog, when set, receives step output (stdout/stderr) lines. This is the
// valuable signal users want to see, so the entrypoint wires it unconditionally
// (unlike DebugLog, which is gated behind ?debug=1).
var OutputLog func(string)

func dbg(format string, args ...any) {
	if DebugLog != nil {
		DebugLog(fmt.Sprintf(format, args...))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// logWriter routes job step output (stdout/stderr) to the debug channel so it
// surfaces in the browser. It never wraps an *os.File, so wazero treats it as a
// plain in-memory writer instead of statting a real fd (which fails on js).
type logWriter struct{}

func (w logWriter) Write(p []byte) (int, error) {
	if OutputLog != nil {
		OutputLog(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
