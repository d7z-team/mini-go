package runtime

import (
	"encoding/json"
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestVMRunsExportedConstantFunction(t *testing.T) {
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

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	result, err := runTestModuleExport(vm, "Main")
	if err != nil {
		t.Fatalf("run export failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}

func TestExecutionStartsAndCompletesExport(t *testing.T) {
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

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	if handle.State() != ExecutionCompleted {
		t.Fatalf("expected completed run, got %s", handle.State())
	}
	result, err := handle.resultValues()
	if err != nil {
		t.Fatalf("vmResult failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
	if _, ok := handle.PauseEvent(); ok {
		t.Fatal("unexpected pause event")
	}
}

func TestExecutionCapturesFailure(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.message", Type: testType("String"), Value: json.RawMessage(`"failed"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.message"},
		}, {
			Op: ir.OpPanic,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	handle, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	if handle.State() != ExecutionFailed {
		t.Fatalf("expected failed run, got %s", handle.State())
	}
	if handle.Err() == nil {
		t.Fatal("expected run error")
	}
	if _, err := handle.resultValues(); err == nil {
		t.Fatal("expected vmResult to return run error")
	}
	if _, err := handle.instance.Start("Main"); !errors.Is(err, ErrInstanceFaulted) {
		t.Fatalf("start after panic = %v", err)
	}
}

func TestVMRunsJSONArtifact(t *testing.T) {
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
	data, err := ir.EncodeJSON(&artifact)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}

	vm, err := loadTestEngineJSON(data)
	if err != nil {
		t.Fatalf("load test engine JSON failed: %v", err)
	}
	result, err := runTestModuleExport(vm, "Main")
	if err != nil {
		t.Fatalf("run export failed: %v", err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(42)))
}
