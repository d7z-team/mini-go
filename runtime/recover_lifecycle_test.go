package runtime

import (
	"context"
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestRecoveredPanicSurvivesNestedDeferredCalls(t *testing.T) {
	for _, nestedPanic := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal cleanup", true: "nested recovered panic"}[nestedPanic], func(t *testing.T) {
			artifact := ir.NewArtifact("recover/lifecycle", "main")
			artifact.Constants = []ir.Constant{{ID: "message", Type: testType("String"), Value: json.RawMessage(`"failed"`)}}
			artifact.Functions = []ir.Function{
				{ID: "fn.entry", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"function() Void", "String"}, []ir.Instruction{
					{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "recover"}},
					{Op: ir.OpDeferPush, Payload: ir.DeferPayload{}},
					{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "message"}},
					{Op: ir.OpPanic},
				}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}})},
				{ID: "recover", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"Any"}, []ir.Instruction{
					{Op: ir.OpRecover},
					{Op: ir.OpPop},
					{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "helper", ArgCount: 0, ResultCount: 0}},
					{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 0}},
				}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}, {nil, nil}})},
				{ID: "helper", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"function() Void"}, []ir.Instruction{
					{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "cleanup"}},
					{Op: ir.OpDeferPush, Payload: ir.DeferPayload{}},
					{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 0}},
				}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}})},
				{ID: "cleanup", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"Any"}, []ir.Instruction{
					{Op: ir.OpRecover},
					{Op: ir.OpPop},
					{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 0}},
				}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}})},
			}
			if nestedPanic {
				code := artifact.Functions[2].Code
				code.Instructions = code.Instructions[:2]
				code.Types = append(code.Types, testType("String"))
				appendTestSlotCode(code, []ir.Instruction{
					{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "message"}},
					{Op: ir.OpPanic},
				}, [][2][]uint32{{nil, {1}}, {{1}, nil}})
			}
			instance, err := patchTestProgram(t, artifact, "recover-lifecycle").Instantiate(context.Background(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			if _, err := instance.Call(context.Background(), "run"); err != nil {
				t.Fatalf("nested cleanup lost the recovered outer panic: %v", err)
			}
		})
	}
}
