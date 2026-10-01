package runtime

import (
	"encoding/json"
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDebuggerSchemaPanic(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.message", Type: testType("String"), Value: json.RawMessage(`"failed"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Locals:    []ir.Local{{ID: "local.msg", Type: testType("String")}},
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.message"},
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.msg"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.msg"},
		}, {
			Op: ir.OpPanic,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.main", pc: 3, line: 9, column: 2})

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	_, err = runTestModuleExport(vm, "Main")
	if err == nil {
		t.Fatal("expected panic error")
	}
	var panicErr panicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("expected PanicError, got %T: %v", err, err)
	}
	events := debugger.Events()
	if len(events) != 1 {
		t.Fatalf("expected one schema event, got %#v", events)
	}
	event := events[0]
	requireDebugEvent(t, event, EventPanic, "fn.main", 3, 9)
	if event.Panic == nil {
		t.Fatal("expected panic value")
	}
	requireDebugString(t, *event.Panic, "failed")
	requireDebugString(t, event.Frame.Locals[0].Value, "failed")
}

func TestDebuggerSchemaDeferredReturn(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
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
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.cleanup", pc: 0, line: 8, column: 2})

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
	if handle.State() != ExecutionPaused {
		t.Fatalf("expected deferred pause, got %s", handle.State())
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected schema deferred pause event")
	}
	requireDebugEventWithContext(t, event, EventBreakpoint, "fn.cleanup", 0, 8, 1)
	if len(event.Stack) != 2 || event.Stack[1].FunctionID != "fn.main" || event.Stack[1].ExecutionContextID != 1 {
		t.Fatalf("expected deferred stack to include owner frame in context 1, got %#v", event.Stack)
	}
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}

func TestDebuggerSchemaDeferredRecover(t *testing.T) {
	artifact := deferredPanicTestArtifact(testSlotCode([]string{"Any"}, []ir.Instruction{
		{Op: ir.OpRecover},
		{Op: ir.OpPop},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 0}},
	}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}))
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.cleanup", pc: 0, line: 8, column: 2})

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
		t.Fatal("expected schema deferred recover pause event")
	}
	requireDebugEventWithContext(t, event, EventBreakpoint, "fn.cleanup", 0, 8, 1)
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values)
	if events := debugger.Events(); len(events) != 1 || events[0].Kind != EventBreakpoint {
		t.Fatalf("expected only recovered breakpoint event, got %#v", events)
	}
}
