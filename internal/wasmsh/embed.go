package wasmsh

import _ "embed"

//go:embed wasmsh.wasm
var binary []byte

// Bytes returns the embedded wasmsh shell compiled to wasm32-wasip1.
func Bytes() []byte {
	return binary
}
