package runtime

import (
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDebuggerSchemaBreakpoint(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Locals:    []ir.Local{{ID: "local.answer", Type: testType("Int64")}},
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.answer"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.answer"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.main", pc: 2, line: 5, column: 1})

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	setTestBreakpoints(t, vm, "example/module", "main.mgo", 5)
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	if handle.State() != ExecutionPaused {
		t.Fatalf("expected paused run, got %s", handle.State())
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected schema pause event")
	}
	requireDebugEvent(t, event, EventBreakpoint, "fn.main", 2, 5)
	requireDebugInt64(t, event.Frame.Locals[0].Value, 42)
	events := debugger.Events()
	if len(events) == 0 {
		t.Fatal("expected recorded schema event")
	}
	next := events[len(events)-1]
	requireDebugEvent(t, next, EventBreakpoint, "fn.main", 2, 5)
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}

func TestDebuggerSchemaStep(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Locals:    []ir.Local{{ID: "local.answer", Type: testType("Int64")}},
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.answer"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.answer"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact,
		testInstructionLocation{function: "fn.main", pc: 0, line: 4, column: 1},
		testInstructionLocation{function: "fn.main", pc: 1, line: 5, column: 1},
	)

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	setTestBreakpoints(t, vm, "example/module", "main.mgo", 4)
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	if _, err := stepIntoTestExecution(handle); err != nil {
		t.Fatalf("StepInto failed: %v", err)
	}
	if handle.State() != ExecutionPaused {
		t.Fatalf("StepInto state = %s, want %s", handle.State(), ExecutionPaused)
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected schema step event")
	}
	requireDebugEvent(t, event, EventStep, "fn.main", 1, 5)
	requireDebugInt64(t, event.Frame.Locals[0].Value, 0)
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}

func TestDebuggerSchemaStepOut(t *testing.T) {
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
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact,
		testInstructionLocation{function: "fn.main", pc: 0, line: 4, column: 1},
		testInstructionLocation{function: "fn.main", pc: 1, line: 5, column: 1},
		testInstructionLocation{function: "fn.inner", pc: 0, line: 8, column: 2},
	)

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	setTestBreakpoints(t, vm, "example/module", "main.mgo", 4)
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	if _, err := stepIntoTestExecution(handle); err != nil {
		t.Fatalf("StepInto failed: %v", err)
	}
	if handle.State() != ExecutionPaused {
		t.Fatalf("StepInto state = %s, want %s", handle.State(), ExecutionPaused)
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected schema step-into event")
	}
	requireDebugEvent(t, event, EventStep, "fn.inner", 0, 8)
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
	requireDebugEvent(t, event, EventStep, "fn.main", 1, 5)
	if len(event.Stack) != 1 {
		t.Fatalf("expected step-out stack to contain only caller frame, got %#v", event.Stack)
	}
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}

func TestDebuggerSchemaRequestedPause(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.main", pc: 0, line: 4, column: 1})

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	if err := vm.requestPause(); err != nil {
		t.Fatalf("RequestPause failed: %v", err)
	}
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	if handle.State() != ExecutionPaused {
		t.Fatalf("expected paused run, got %s", handle.State())
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected schema pause event")
	}
	requireDebugEvent(t, event, EventPause, "fn.main", 0, 4)
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}
