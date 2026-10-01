package runtime

import (
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDebuggerSchemaStepOutFromDeferredCleanup(t *testing.T) {
	for _, test := range []struct {
		name         string
		recoverPanic bool
	}{
		{name: "return"},
		{name: "recover", recoverPanic: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := ir.NewArtifact("example/module", "main")
			artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
			artifact.Functions = []ir.Function{{
				ID:        "fn.main",
				Signature: testSignature("function() Int64"),
				Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
					Op:      ir.OpCallDirect,
					Payload: ir.CallPayload{Function: "fn.inner", ArgCount: 0, ResultCount: 1},
				}, {
					Op:      ir.OpReturn,
					Payload: ir.ReturnPayload{ResultCount: 1},
				}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
			}, {
				ID:        "fn.inner",
				Signature: testSignature("function() Int64"),
				Code: testSlotCode([]string{"function() Void", "Int64"}, []ir.Instruction{{
					Op:      ir.OpMakeClosure,
					Payload: ir.ClosurePayload{Function: "fn.cleanup"},
				}, {
					Op: ir.OpDeferPush, Payload: ir.DeferPayload{},
				}, {
					Op:      ir.OpConst,
					Payload: ir.ConstPayload{Constant: "c.answer"},
				}, {
					Op:      ir.OpReturn,
					Payload: ir.ReturnPayload{ResultCount: 1},
				}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
			}, {
				ID:        "fn.cleanup",
				Signature: testSignature("function() Void"),
				Code: testSlotCode([]string{}, []ir.Instruction{{
					Op:      ir.OpReturn,
					Payload: ir.ReturnPayload{ResultCount: 0},
				}}, [][2][]uint32{{nil, nil}}),
			}}
			if test.recoverPanic {
				artifact.Constants = append(artifact.Constants, ir.Constant{
					ID: "c.message", Type: testType("String"), Value: json.RawMessage(`"failed"`),
				})
				artifact.Functions[0].Code = testSlotCode([]string{"Int64"}, []ir.Instruction{
					{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.inner", ArgCount: 0, ResultCount: 0}},
					{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.answer"}},
					{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
				}, [][2][]uint32{{nil, nil}, {nil, {0}}, {{0}, nil}})
				inner := &artifact.Functions[1]
				inner.Signature = testSignature("function() Void")
				inner.Code.Types[1] = testType("String")
				inner.Code.Descriptors.Const[0] = ir.ConstPayload{Constant: "c.message"}
				inner.Code.Instructions[3].Op = ir.OpPanic
				inner.Code.Instructions[3].Descriptor = 0
				artifact.Functions[2].Code = testSlotCode([]string{"Any"}, []ir.Instruction{
					{Op: ir.OpRecover},
					{Op: ir.OpPop},
					{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 0}},
				}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}})
			}
			artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
			setTestInstructionLocations(t, &artifact,
				testInstructionLocation{function: "fn.main", pc: 1, line: 5, column: 1},
				testInstructionLocation{function: "fn.cleanup", pc: 0, line: 8, column: 2},
			)

			debugger := NewDebugger()
			vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
			if err != nil {
				t.Fatalf("load test engine failed: %v", err)
			}
			setTestBreakpoints(t, vm, "example/module", "main.mgo", 8)
			handle, err := startTestExecution(vm, "Main")
			if err != nil {
				t.Fatalf("start execution failed: %v", err)
			}
			event, ok := handle.PauseEvent()
			if !ok {
				t.Fatal("expected schema deferred cleanup pause event")
			}
			expectDeferred := ExpectedEvent{
				Kind:               EventBreakpoint,
				RunID:              1,
				ExecutionContextID: 1,
				FunctionID:         "fn.cleanup",
				PC:                 0,
				Line:               8,
				Column:             2,
				Stack: []ExpectedFrame{{
					ExecutionContextID: 1,
					FunctionID:         "fn.cleanup",
					PC:                 0,
					Line:               8,
					Column:             2,
				}, {
					ExecutionContextID: 1,
					FunctionID:         "fn.inner",
					PC:                 3,
				}, {
					ExecutionContextID: 1,
					FunctionID:         "fn.main",
					PC:                 0,
				}},
			}
			if err := expectDeferred.Match(event); err != nil {
				t.Fatalf("deferred breakpoint validation failed: %v", err)
			}
			if _, err := stepOutTestExecution(handle); err != nil {
				t.Fatalf("StepOut failed: %v", err)
			}
			if handle.State() != ExecutionPaused {
				t.Fatalf("StepOut state = %s, want %s", handle.State(), ExecutionPaused)
			}
			event, ok = handle.PauseEvent()
			if !ok {
				t.Fatal("expected schema step-out event")
			}
			expectStepOut := ExpectedEvent{
				Kind:               EventStep,
				RunID:              1,
				ExecutionContextID: 1,
				FunctionID:         "fn.main",
				PC:                 1,
				Line:               5,
				Column:             1,
				Stack: []ExpectedFrame{{
					ExecutionContextID: 1,
					FunctionID:         "fn.main",
					PC:                 1,
					Line:               5,
					Column:             1,
				}},
			}
			if err := expectStepOut.Match(event); err != nil {
				t.Fatalf("deferred step-out validation failed: %v", err)
			}
			result, err := continueTestExecution(handle)
			if err != nil {
				t.Fatalf("Continue failed: %v", err)
			}
			requireValues(t, result.Values, newVMValue("Int64", int64(42)))
			for _, event := range debugger.Events() {
				if event.Kind == EventPanic {
					t.Fatalf("completed deferred cleanup produced panic event: %#v", event)
				}
			}
		})
	}
}
