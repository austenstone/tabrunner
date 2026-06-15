package runner

import "fmt"

// DebugLog, when set, receives human-readable diagnostic lines from the step
// execution loop. Wired by the js/wasm entrypoint to the UI status channel.
var DebugLog func(string)

func dbg(format string, args ...any) {
	if DebugLog != nil {
		DebugLog(fmt.Sprintf(format, args...))
	}
}
