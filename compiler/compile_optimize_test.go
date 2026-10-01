package compiler

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileEmitsTailCallForExactDirectReturn(t *testing.T) {
	result, err := compileTestSource("example/tail", "tail.mgo", `package tail

func target(value int) int { return value }
func wrapper(value int) int { return target(value) }
`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	if function, _, ok := artifactFunctionByName(result.Artifact, result.Symbols, "wrapper"); ok {
		for _, instruction := range functionOperations(t, function) {
			if instruction.Op == ir.OpTailCallDirect {
				return
			}
		}
		t.Fatalf("wrapper instructions = %#v", functionOperations(t, function))
	}
	t.Fatal("missing wrapper function")
}
