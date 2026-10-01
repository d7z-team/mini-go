package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestVMSetIndexMapUsesKeyValueTypes(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{
		{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"u"`)},
		{ID: "c.name", Type: testType("String"), Value: json.RawMessage(`"Ada"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Locals:    []ir.Local{{ID: "local.users", Type: testType("Map<String, User>")}},
		Code: testSlotCode([]string{"Map<String, User>", "String", "String", "User"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 0),
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.users"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.users"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.name"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("User", "Name"),
		}, {
			Op: ir.OpStoreIndex,
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.users"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "Name"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {nil, {1}}, {nil, {2}}, {{2}, {3}}, {{0, 1, 3}, nil}, {nil, {0}}, {nil, {1}}, {{0, 1}, {3}}, {{3}, {1}}, {{1}, nil}}),
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
	requireValues(t, result.Values, newVMValue("String", "Ada"))
}

func TestLoaderSetIndexMapRejectsWrongValueType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{
		{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"u"`)},
		{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Map<String, User>", "String", "String"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op: ir.OpStoreIndex,
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {nil, {2}}, {{0, 1, 2}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to example/module.User") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderMapIndexRejectsWrongKeyType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("Int64"), Value: json.RawMessage(`1`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Map<String, User>", "Int64", "User"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "Int64 is not assignable to String") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderDeleteMapRejectsWrongKeyType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("Int64"), Value: json.RawMessage(`1`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Map<String, User>", "Int64"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op: ir.OpDelete,
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "Int64 is not assignable to String") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestVMMapKeysUsesMapKeyType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"u"`)},
		{ID: "c.name", Type: testType("String"), Value: json.RawMessage(`"Ada"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Locals:    []ir.Local{{ID: "local.keys", Type: testType("Slice<String>")}},
		Code: testSlotCode([]string{"String", "String", "User", "Map<String, User>", "Slice<String>", "Int64"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.name"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("User", "Name"),
		}, {
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 1),
		}, {
			Op: ir.OpMapKeys,
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.keys"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.keys"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.zero"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{1}, {2}}, {{0, 2}, {3}}, {{3}, {4}}, {{4}, nil}, {nil, {4}}, {nil, {5}}, {{4, 5}, {0}}, {{0}, nil}}),
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
	requireValues(t, result.Values, newVMValue("String", "u"))
}

func TestVMMapIndexMissingNamedStructReturnsZeroValue(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"missing"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Code: testSlotCode([]string{"Map<String, User>", "String", "User"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "Name"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, {1}}, {{1}, nil}}),
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
	requireValues(t, result.Values, newVMValue("String", ""))
}

func TestVMMapIndexMissingPrimitiveReturnsZeroValue(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"missing"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Map<String, Int64>", "String", "Int64"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, Int64>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
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
	requireValues(t, result.Values, newVMValue("Int64", int64(0)))
}

func TestVMLoadIndexOKReturnsValueAndPresence(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"u"`)},
		{ID: "c.value", Type: testType("Int64"), Value: json.RawMessage(`42`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() tuple(Int64, Bool)"),
		Code: testSlotCode([]string{"String", "Int64", "Map<String, Int64>", "Bool"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.value"},
		}, {
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, Int64>", 1),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op: ir.OpLoadIndexOK,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 2},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {nil, {0}}, {{2, 0}, {1, 3}}, {{1, 3}, nil}}),
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
	requireValues(t, result.Values, newVMValue("Int64", int64(42)), newVMValue("Bool", true))
}

func TestVMLoadIndexOKMissingReturnsZeroValueAndFalse(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"missing"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() tuple(Int64, Bool)"),
		Code: testSlotCode([]string{"Map<String, Int64>", "String", "Int64", "Bool"}, []ir.Instruction{{
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, Int64>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op: ir.OpLoadIndexOK,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 2},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2, 3}}, {{2, 3}, nil}}),
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
	requireValues(t, result.Values, newVMValue("Int64", int64(0)), newVMValue("Bool", false))
}
