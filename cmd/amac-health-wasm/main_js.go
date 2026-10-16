//go:build js && wasm

// The browser bridge accepts sample JSON and returns reports from AMAC's
// shared Go evaluator. It has no access to host probes or browser storage.
package main

import (
	"encoding/json"
	"syscall/js"
	"time"

	"github.com/lgoyal6/amac/internal/health/demo"
)

func main() {
	js.Global().Set("amacReady", false)
	js.Global().Set("amacEvaluate", js.FuncOf(evaluate))
	js.Global().Set("amacReady", true)
	select {}
}

func evaluate(_ js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeString {
		return `{"error":"Pass one JSON string"}`
	}
	reports, err := demo.Evaluate([]byte(args[0].String()), time.Now())
	if err != nil {
		encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(encoded)
	}
	encoded, err := json.Marshal(map[string]any{"reports": reports})
	if err != nil {
		return `{"error":"Could not encode health reports"}`
	}
	return string(encoded)
}
