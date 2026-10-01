package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLoaderStoreLocalRejectsWrongDeclaredType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Locals:    []ir.Local{{ID: "local.value", Type: testType("Int64")}},
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.value"},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to Int64") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderStoreGlobalRejectsWrongDeclaredType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)}}
	artifact.Globals = []ir.Global{{ID: "global.value", Type: testType("Int64")}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpStoreGlobal,
			Payload: ir.GlobalPayload{Global: "global.value"},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to Int64") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderFunctionArgumentRejectsWrongDeclaredType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)}}
	artifact.Functions = []ir.Function{{
		Code:      &ir.SlotCode{},
		ID:        "fn.accept",
		Signature: testSignature("function(Int64) Void"),
		Locals:    []ir.Local{{ID: "local.value", Type: testType("Int64")}},
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpCallDirect,
			Payload: ir.CallPayload{Function: "fn.accept", ArgCount: 1, ResultCount: 0},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to Int64") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderReturnRejectsWrongDeclaredType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to Int64") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestVMFunctionFallthroughRejectsMissingReturnValue(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Functions = []ir.Function{{
		Code:      &ir.SlotCode{},
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	_, err = runTestModuleExport(vm, "Main")
	if err == nil || !strings.Contains(err.Error(), "return value count mismatch") {
		t.Fatalf("expected missing return value error, got %v", err)
	}
}

func TestVMReturnNamedStructCopiesValue(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.name", Type: testType("String"), Value: json.RawMessage(`"Ada"`)}}
	artifact.Globals = []ir.Global{{ID: "global.user", Type: testType("User")}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.init",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"String", "User"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.name"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("User", "Name"),
		}, {
			Op:      ir.OpStoreGlobal,
			Payload: ir.GlobalPayload{Global: "global.user"},
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() User"),
		Code: testSlotCode([]string{"User"}, []ir.Instruction{{
			Op:      ir.OpLoadGlobal,
			Payload: ir.GlobalPayload{Global: "global.user"},
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
	if _, err := storeFieldValue(vm.rootModule(), first.Values[0], "Name", newVMValue("String", "Eve")); err != nil {
		t.Fatalf("mutate returned struct: %v", err)
	}
	second, err := runTestModuleExport(vm, "Main")
	if err != nil {
		t.Fatalf("second run export failed: %v", err)
	}
	got, err := loadFieldValue(vm.rootModule(), second.Values[0], "Name")
	if err != nil {
		t.Fatalf("load returned field: %v", err)
	}
	if got.Type.String() != "String" || got.Data != "Ada" {
		t.Fatalf("return value mutation leaked into VM global, got %#v", got)
	}
}
