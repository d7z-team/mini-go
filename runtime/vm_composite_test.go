package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestVMRunsCompositeAccess(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
		{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64", "Int64", "Slice<Int64>"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op:      ir.OpMakeSequence,
			Payload: testArrayPayload("Slice<Int64>", 2),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {nil, {0}}, {{2, 0}, {1}}, {{1}, nil}}),
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

func TestVMRunsCompositeMutation(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
		{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Locals:    []ir.Local{{ID: "local.arr", Type: testType("Slice<Int64>")}},
		Code: testSlotCode([]string{"Int64", "Int64", "Slice<Int64>"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.zero"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpMakeSequence,
			Payload: testArrayPayload("Slice<Int64>", 2),
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.arr"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.arr"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op: ir.OpStoreIndex,
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.arr"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}, {nil, {2}}, {nil, {0}}, {nil, {1}}, {{2, 0, 1}, nil}, {nil, {2}}, {nil, {0}}, {{2, 0}, {1}}, {{1}, nil}}),
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

func TestVMRunsStructMember(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "Point",
		Type: testType("struct{X:Int64}"),
		Fields: []testTypeField{{
			Name: "X",
			Type: testType("Int64"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64", "Point"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.answer"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("Point", "X"),
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "X"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, {0}}, {{0}, nil}}),
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

func TestVMZeroNamedStructUsesTypeFieldMetadata(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{ID:Int64 `json:\"id\"`,Name:String `json:\"name\"`}"),
		Fields: []testTypeField{{
			Name: "ID",
			Type: testType("Int64"),
			Tag:  `json:"id"`,
		}, {
			Name: "Name",
			Type: testType("String"),
			Tag:  `json:"name"`,
		}},
	}})
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Code: testSlotCode([]string{"User", "String"}, []ir.Instruction{{
			Op:      ir.OpZero,
			Payload: testTypePayload("User"),
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "Name"},
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
	requireValues(t, result.Values, newVMValue("String", ""))
}

func TestVMZeroNamedStructMetadataHandlesRecursiveType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "Node",
		Type: testType("struct{Next:Ptr<Node>,Label:String}"),
		Fields: []testTypeField{{
			Name: "Next",
			Type: testType("Ptr<Node>"),
		}, {
			Name: "Label",
			Type: testType("String"),
		}},
	}})

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	value := vm.rootModule().zeroValue("Node")
	next, err := loadFieldValue(vm.rootModule(), value, "Next")
	if err != nil {
		t.Fatal(err)
	}
	if next.Type.String() != "Ptr<example/module.Node>" || next.Data != nil {
		t.Fatalf("expected recursive pointer field to be nil, got %#v", next)
	}
	label, err := loadFieldValue(vm.rootModule(), value, "Label")
	if err != nil {
		t.Fatal(err)
	}
	requireValues(t, []vmValue{label}, newVMValue("String", ""))
}

func TestVMNamedStructMemberReadUsesTypeFieldMetadata(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{ID:Int64,Name:String}"),
		Fields: []testTypeField{{
			Name: "ID",
			Type: testType("Int64"),
		}, {
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.id", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Code: testSlotCode([]string{"Int64", "User", "String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.id"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("User", "ID"),
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "Name"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, {2}}, {{2}, nil}}),
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

func TestLoaderNamedStructMakeStructRejectsUnknownField(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.age", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int64", "User"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.age"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("User", "Age"),
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), `unknown field "Age" for example/module.User`) {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderNamedStructMakeStructRejectsWrongFieldType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int64", "User"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpMakeStruct,
			Payload: testStructPayload("User", "Name"),
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "Int64 is not assignable to String") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestVMNamedStructStoreFieldUsesTypeFieldMetadata(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{ID:Int64,Name:String}"),
		Fields: []testTypeField{{
			Name: "ID",
			Type: testType("Int64"),
		}, {
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.name", Type: testType("String"), Value: json.RawMessage(`"Ada"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Locals:    []ir.Local{{ID: "local.user", Type: testType("User")}},
		Code: testSlotCode([]string{"User", "String"}, []ir.Instruction{{
			Op:      ir.OpZero,
			Payload: testTypePayload("User"),
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.user"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.user"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.name"},
		}, {
			Op:      ir.OpStoreField,
			Payload: ir.FieldPayload{Field: "Name"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.user"},
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "Name"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {nil, {1}}, {{0, 1}, nil}, {nil, {0}}, {{0}, {1}}, {{1}, nil}}),
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

func TestLoaderNamedStructStoreFieldRejectsWrongType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"User", "Int64"}, []ir.Instruction{{
			Op:      ir.OpZero,
			Payload: testTypePayload("User"),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpStoreField,
			Payload: ir.FieldPayload{Field: "Name"},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "Int64 is not assignable to String") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}
