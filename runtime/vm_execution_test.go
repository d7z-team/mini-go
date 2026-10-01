package runtime

import (
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestVMRunsDirectCallWithLocals(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.input", Type: testType("String"), Value: json.RawMessage(`"ok"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.echo",
		Signature: testSignature("function(String) String"),
		Locals:    []ir.Local{{ID: "local.value", Type: testType("String")}},
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.value"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Code: testSlotCode([]string{"String", "String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.input"},
		}, {
			Op:      ir.OpCallDirect,
			Payload: ir.CallPayload{Function: "fn.echo", ArgCount: 1, ResultCount: 1},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
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
	requireValues(t, result.Values, newVMValue("String", "ok"))
}

func TestVMTailCallDoesNotConsumeCallDepth(t *testing.T) {
	artifact := ir.NewArtifact("example/tail", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.depth", Type: testType("Int64"), Value: json.RawMessage(`5000`)},
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
	}
	artifact.Functions = []ir.Function{
		{
			ID: "fn.count", Signature: testSignature("function(Int64) Int64"),
			Locals: []ir.Local{{ID: "local.n", Type: testType("Int64")}},
			Code: testSlotCode([]string{"Int64", "Int64", "Bool", "Int64"}, []ir.Instruction{
				{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "local.n"}},
				{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.zero"}},
				{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "=="}},
				{Op: ir.OpJumpIf, Payload: ir.JumpPayload{Label: "done"}},
				{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "local.n"}},
				{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.one"}},
				{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "-"}},
				{Op: ir.OpTailCallDirect, Payload: ir.CallPayload{Function: "fn.count", ArgCount: 1, ResultCount: 1}},
				{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "done"}},
				{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "local.n"}},
				{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
			}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}, {nil, {0}}, {nil, {1}}, {{0, 1}, {3}}, {{3}, nil}, {nil, nil}, {nil, {0}}, {{0}, nil}}),
		},
		{
			ID: "fn.main", Signature: testSignature("function() Int64"),
			Code: testSlotCode([]string{"Int64", "Int64"}, []ir.Instruction{
				{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.depth"}},
				{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.count", ArgCount: 1, ResultCount: 1}},
				{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
			}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
		},
	}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Limits: Limits{MaxCallDepth: 4}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTestModuleExport(vm, "Main")
	if err != nil {
		t.Fatal(err)
	}
	requireValues(t, result.Values, newVMValue("Int64", int64(0)))
}

func TestVMRunsConditionalJump(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.true", Type: testType("Bool"), Value: json.RawMessage(`true`)},
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Bool", "Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.true"},
		}, {
			Op:      ir.OpJumpIf,
			Payload: ir.JumpPayload{Label: "truthy"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.zero"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}, {
			Op:      ir.OpLabel,
			Payload: ir.LabelPayload{Label: "truthy"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}, {nil, nil}, {nil, {1}}, {{1}, nil}}),
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
	requireValues(t, result.Values, newVMValue("Int64", int64(1)))
}

func TestVMRunsBinaryAndUnaryOperators(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.two", Type: testType("Int64"), Value: json.RawMessage(`2`)},
		{ID: "c.three", Type: testType("Int64"), Value: json.RawMessage(`3`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Bool"),
		Code: testSlotCode([]string{"Int64", "Int64", "Int64", "Bool", "Bool"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.two"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.three"},
		}, {
			Op:      ir.OpBinary,
			Payload: ir.OperatorPayload{Operator: "+"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.three"},
		}, {
			Op:      ir.OpBinary,
			Payload: ir.OperatorPayload{Operator: ">"},
		}, {
			Op:      ir.OpUnary,
			Payload: ir.OperatorPayload{Operator: "!"},
		}, {
			Op:      ir.OpUnary,
			Payload: ir.OperatorPayload{Operator: "!"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {nil, {0}}, {{2, 0}, {3}}, {{3}, {4}}, {{4}, {3}}, {{3}, nil}}),
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
	requireValues(t, result.Values, newVMValue("Bool", true))
}

func TestVMStoresAndLoadsGlobal(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.value", Type: testType("Int64"), Value: json.RawMessage(`7`)}}
	artifact.Globals = []ir.Global{{ID: "global.value", Type: testType("Int64")}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.value"},
		}, {
			Op:      ir.OpStoreGlobal,
			Payload: ir.GlobalPayload{Global: "global.value"},
		}, {
			Op:      ir.OpLoadGlobal,
			Payload: ir.GlobalPayload{Global: "global.value"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, nil}}),
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
	requireValues(t, result.Values, newVMValue("Int64", int64(7)))
}

func TestVMRunModuleExportRunsRootInitOnce(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
	}
	artifact.Globals = []ir.Global{{ID: "global.count", Type: testType("Int64")}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.init",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int64", "Int64", "Int64"}, []ir.Instruction{{
			Op:      ir.OpLoadGlobal,
			Payload: ir.GlobalPayload{Global: "global.count"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpBinary,
			Payload: ir.OperatorPayload{Operator: "+"},
		}, {
			Op:      ir.OpStoreGlobal,
			Payload: ir.GlobalPayload{Global: "global.count"},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{{
			Op:      ir.OpLoadGlobal,
			Payload: ir.GlobalPayload{Global: "global.count"},
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
	first, err := runTestModuleExport(vm, "Main")
	if err != nil {
		t.Fatalf("first run export failed: %v", err)
	}
	second, err := runTestModuleExport(vm, "Main")
	if err != nil {
		t.Fatalf("second run export failed: %v", err)
	}
	requireValues(t, first.Values, newVMValue("Int64", int64(1)))
	requireValues(t, second.Values, newVMValue("Int64", int64(1)))
}
