//go:build js && wasm

package main

import (
	"fmt"
	"syscall/js"

	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
)

var toolsCall js.Func

func main() {
	toolsCall = js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		if len(arguments) != 1 || arguments[0].Type() != js.TypeObject {
			panic("compiler call requires one Uint8Array")
		}
		input := make([]byte, arguments[0].Get("byteLength").Int())
		if copied := js.CopyBytesToGo(input, arguments[0]); copied != len(input) {
			panic(fmt.Sprintf("copied %d of %d compiler input bytes", copied, len(input)))
		}
		output := compilerentry.Tools(input)
		result := js.Global().Get("Uint8Array").New(len(output))
		if copied := js.CopyBytesToJS(result, output); copied != len(output) {
			panic(fmt.Sprintf("copied %d of %d compiler output bytes", copied, len(output)))
		}
		return result
	})
	js.Global().Set("__miniGoToolsCall", toolsCall)
	select {}
}
