package runtime

import (
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDebuggerSchemaSpawnedContext(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Instructions: []ir.Instruction{{
			Op:      string(ir.OpMakeClosure),
			Payload: json.RawMessage(`{"function":"fn.child"}`),
		}, {
			Op:      string(ir.OpSpawn),
			Payload: json.RawMessage(`{"arg_count":0}`),
		}, {
			Op:      string(ir.OpConst),
			Payload: json.RawMessage(`{"constant":"c.answer"}`),
		}, {
			Op:      string(ir.OpReturn),
			Payload: json.RawMessage(`{"result_count":1}`),
		}},
	}, {
		ID:        "fn.child",
		Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{{
			Op:      string(ir.OpReturn),
			Payload: json.RawMessage(`{"result_count":0}`),
		}},
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.child", pc: 0, line: 8, column: 2})

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
		t.Fatal("expected schema spawned pause event")
	}
	requireDebugEventWithContext(t, event, EventBreakpoint, "fn.child", 0, 8, 2)
	if len(event.Stack) != 2 || event.Stack[0].ExecutionContextID != 2 || event.Stack[1].ExecutionContextID != 1 {
		t.Fatalf("expected child context 2 with parent context 1 stack, got %#v", event.Stack)
	}
	result, err := continueTestExecution(handle)
	if err != nil {
		t.Fatalf("Continue failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}

func TestStepAfterSpawnStaysWithTheSelectedTask(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Functions = []ir.Function{
		{ID: "fn.main", Signature: testSignature("function() Void"), Instructions: []ir.Instruction{
			{Op: string(ir.OpMakeClosure), Payload: json.RawMessage(`{"function":"fn.child"}`)},
			{Op: string(ir.OpSpawn), Payload: json.RawMessage(`{"arg_count":0}`)},
			{Op: string(ir.OpReturn), Payload: json.RawMessage(`{"result_count":0}`)},
		}},
		{ID: "fn.child", Signature: testSignature("function() Void"), Instructions: []ir.Instruction{
			{Op: string(ir.OpReturn), Payload: json.RawMessage(`{"result_count":0}`)},
		}},
	}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	setTestInstructionLocations(t, &artifact,
		testInstructionLocation{function: "fn.main", pc: 1, line: 4, column: 1},
		testInstructionLocation{function: "fn.main", pc: 2, line: 5, column: 1},
		testInstructionLocation{function: "fn.child", pc: 0, line: 8, column: 1},
	)
	machine, err := loadTestEngineWithOptions(artifact, InstanceOptions{Debugger: NewDebugger()})
	if err != nil {
		t.Fatal(err)
	}
	setTestBreakpoints(t, machine, "example/module", "main.mgo", 4)
	execution, err := startTestExecution(machine, "Main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepIntoTestExecution(execution); err != nil {
		t.Fatal(err)
	}
	event, ok := execution.PauseEvent()
	if !ok {
		t.Fatal("selected task did not stop after spawn")
	}
	requireDebugEventWithContext(t, event, EventStep, "fn.main", 2, 5, 1)
	if _, err := continueTestExecution(execution); err != nil {
		t.Fatal(err)
	}
}
