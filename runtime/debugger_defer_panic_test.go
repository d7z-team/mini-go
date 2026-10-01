package runtime

import (
	"encoding/json"
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDebuggerSchemaPanicAfterDeferredCleanup(t *testing.T) {
	artifact := deferredPanicTestArtifact(testSlotCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 0}}}, [][2][]uint32{{nil, nil}}))
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
	panicValue := ExpectedString("failed")
	expectPanic := ExpectedEvent{
		Kind:               EventPanic,
		RunID:              1,
		ExecutionContextID: 1,
		FunctionID:         "fn.main",
		PC:                 3,
		Line:               9,
		Column:             2,
		Panic:              &panicValue,
		Stack: []ExpectedFrame{{
			ExecutionContextID: 1,
			FunctionID:         "fn.main",
			PC:                 3,
			Line:               9,
			Column:             2,
		}},
	}
	if err := expectPanic.Match(events[0]); err != nil {
		t.Fatalf("deferred panic validation failed: %v", err)
	}
}

func TestDebuggerSchemaNestedDeferredRecoverThenTail(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.message", Type: testType("String"), Value: json.RawMessage(`"failed"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void", "String"}, []ir.Instruction{{
			Op:      ir.OpMakeClosure,
			Payload: ir.ClosurePayload{Function: "fn.tail"},
		}, {
			Op: ir.OpDeferPush, Payload: ir.DeferPayload{},
		}, {
			Op:      ir.OpMakeClosure,
			Payload: ir.ClosurePayload{Function: "fn.recover"},
		}, {
			Op: ir.OpDeferPush, Payload: ir.DeferPayload{},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.message"},
		}, {
			Op: ir.OpPanic,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}, {
		ID:        "fn.recover",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Any"}, []ir.Instruction{{
			Op: ir.OpRecover,
		}, {
			Op: ir.OpPop,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 0},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}),
	}, {
		ID:        "fn.tail",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{}, []ir.Instruction{{
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 0},
		}}, [][2][]uint32{{nil, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact,
		testInstructionLocation{function: "fn.recover", pc: 0, line: 8, column: 2},
		testInstructionLocation{function: "fn.tail", pc: 0, line: 10, column: 2},
	)

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	setTestBreakpoints(t, vm, "example/module", "main.mgo", 8, 10)
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected first deferred breakpoint")
	}
	expectRecover := ExpectedEvent{
		Kind:               EventBreakpoint,
		RunID:              1,
		ExecutionContextID: 1,
		FunctionID:         "fn.recover",
		PC:                 0,
		Line:               8,
		Column:             2,
		Stack: []ExpectedFrame{{
			ExecutionContextID: 1,
			FunctionID:         "fn.recover",
			PC:                 0,
			Line:               8,
			Column:             2,
		}, {
			ExecutionContextID: 1,
			FunctionID:         "fn.main",
			PC:                 5,
		}},
	}
	if err := expectRecover.Match(event); err != nil {
		t.Fatalf("recover defer event validation failed: %v", err)
	}
	if _, err := continueTestExecution(handle); err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	if handle.State() != ExecutionPaused {
		t.Fatalf("Continue state = %s, want %s", handle.State(), ExecutionPaused)
	}
	event, ok = handle.PauseEvent()
	if !ok {
		t.Fatal("expected tail deferred breakpoint")
	}
	expectTail := ExpectedEvent{
		Kind:               EventBreakpoint,
		RunID:              1,
		ExecutionContextID: 1,
		FunctionID:         "fn.tail",
		PC:                 0,
		Line:               10,
		Column:             2,
		Stack: []ExpectedFrame{{
			ExecutionContextID: 1,
			FunctionID:         "fn.tail",
			PC:                 0,
			Line:               10,
			Column:             2,
		}, {
			ExecutionContextID: 1,
			FunctionID:         "fn.main",
			PC:                 5,
		}},
	}
	if err := expectTail.Match(event); err != nil {
		t.Fatalf("tail defer event validation failed: %v", err)
	}
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values)
	for _, event := range debugger.Events() {
		if event.Kind == EventPanic {
			t.Fatalf("recovered nested defer must not produce panic event: %#v", event)
		}
	}
}

func TestDebuggerSchemaRecoverContextClearedForLaterDefer(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.message", Type: testType("String"), Value: json.RawMessage(`"failed"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void", "String"}, []ir.Instruction{{
			Op:      ir.OpMakeClosure,
			Payload: ir.ClosurePayload{Function: "fn.observe"},
		}, {
			Op: ir.OpDeferPush, Payload: ir.DeferPayload{},
		}, {
			Op:      ir.OpMakeClosure,
			Payload: ir.ClosurePayload{Function: "fn.recover"},
		}, {
			Op: ir.OpDeferPush, Payload: ir.DeferPayload{},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.message"},
		}, {
			Op: ir.OpPanic,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}, {
		ID:        "fn.recover",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Any"}, []ir.Instruction{{
			Op: ir.OpRecover,
		}, {
			Op: ir.OpPop,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 0},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}),
	}, {
		ID:        "fn.observe",
		Signature: testSignature("function() Void"),
		Locals:    []ir.Local{{ID: "local.recovered", Type: testType("Any")}},
		Code: testSlotCode([]string{"Any"}, []ir.Instruction{{
			Op: ir.OpRecover,
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.recovered"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 0},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.observe", pc: 2, line: 10, column: 2})

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	setTestBreakpoints(t, vm, "example/module", "main.mgo", 10)
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	event, ok := handle.PauseEvent()
	if !ok {
		t.Fatal("expected observe deferred breakpoint")
	}
	expectObserve := ExpectedEvent{
		Kind:               EventBreakpoint,
		RunID:              1,
		ExecutionContextID: 1,
		FunctionID:         "fn.observe",
		PC:                 2,
		Line:               10,
		Column:             2,
	}
	if err := expectObserve.Match(event); err != nil {
		t.Fatalf("observe defer event validation failed: %v", err)
	}
	if len(event.Frame.Locals) != 1 {
		t.Fatalf("expected observe local snapshot, got %#v", event.Frame.Locals)
	}
	if err := (ExpectedValue{Kind: ValueNil, Type: "Any", Nil: true}).Match(event.Frame.Locals[0].Value); err != nil {
		t.Fatalf("expected later defer recover value to be nil: %v", err)
	}
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values)
	for _, event := range debugger.Events() {
		if event.Kind == EventPanic {
			t.Fatalf("recovered panic must not produce panic event after later defer: %#v", event)
		}
	}
}

func TestDebuggerSchemaDeferredRepanicReplacesOwnerPanic(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.owner", Type: testType("String"), Value: json.RawMessage(`"owner"`)},
		{ID: "c.replacement", Type: testType("String"), Value: json.RawMessage(`"replacement"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void", "String"}, []ir.Instruction{{
			Op:      ir.OpMakeClosure,
			Payload: ir.ClosurePayload{Function: "fn.repanic"},
		}, {
			Op: ir.OpDeferPush, Payload: ir.DeferPayload{},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.owner"},
		}, {
			Op: ir.OpPanic,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}, {
		ID:        "fn.repanic",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Any", "String"}, []ir.Instruction{{
			Op: ir.OpRecover,
		}, {
			Op: ir.OpPop,
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.replacement"},
		}, {
			Op: ir.OpPanic,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.repanic", pc: 3, line: 8, column: 2})

	debugger := NewDebugger()
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: debugger})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	_, err = runTestModuleExport(vm, "Main")
	if err == nil {
		t.Fatal("expected replacement panic error")
	}
	var panicErr panicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("expected PanicError, got %T: %v", err, err)
	}
	if panicErr.Value.Type.String() != "String" || panicErr.Value.Data != "replacement" {
		t.Fatalf("expected replacement panic value, got %#v", panicErr.Value)
	}
	events := debugger.Events()
	if len(events) != 1 {
		t.Fatalf("expected one replacement panic event, got %#v", events)
	}
	panicValue := ExpectedString("replacement")
	expectPanic := ExpectedEvent{
		Kind:               EventPanic,
		RunID:              1,
		ExecutionContextID: 1,
		FunctionID:         "fn.repanic",
		PC:                 3,
		Line:               8,
		Column:             2,
		Panic:              &panicValue,
		Stack: []ExpectedFrame{{
			ExecutionContextID: 1,
			FunctionID:         "fn.repanic",
			PC:                 3,
			Line:               8,
			Column:             2,
		}, {
			ExecutionContextID: 1,
			FunctionID:         "fn.main",
			PC:                 3,
		}},
	}
	if err := expectPanic.Match(events[0]); err != nil {
		t.Fatalf("replacement panic event validation failed: %v", err)
	}
}
