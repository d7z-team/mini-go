package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLoaderMakeSequenceRejectsWrongElementType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"String", "Slice<User>"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpMakeSequence,
			Payload: testArrayPayload("Slice<User>", 1),
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to example/module.User") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestLoaderMakeMapRejectsWrongValueType(t *testing.T) {
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
		Code: testSlotCode([]string{"String", "String", "Map<String, User>"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.key"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpMakeMap,
			Payload: testMapPayload("Map<String, User>", 1),
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to example/module.User") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}

func TestVMAppendRejectsWrongElementType(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	setRuntimeTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{Name:String}"),
		Fields: []testTypeField{{
			Name: "Name",
			Type: testType("String"),
		}},
	}})
	artifact.Constants = []ir.Constant{{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Slice<User>", "String", "Slice<User>"}, []ir.Instruction{{
			Op:      ir.OpMakeSequence,
			Payload: testArrayPayload("Slice<User>", 0),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpAppend,
			Payload: ir.CountPayload{Count: 1},
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	_, err = runTestModuleExport(vm, "Main")
	if err == nil || !strings.Contains(err.Error(), "append element 0") || !strings.Contains(err.Error(), "String is not example/module.User") {
		t.Fatalf("expected append element type error, got %v", err)
	}
}

func TestVMAppendExpandsEveryUTF8Byte(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.text", Type: testType("String"), Value: json.RawMessage(`"α"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Slice<Uint8>"),
		Code: testSlotCode([]string{"Slice<Uint8>", "String", "Slice<Uint8>"}, []ir.Instruction{{
			Op: ir.OpMakeSequence, Payload: testArrayPayload("Slice<Uint8>", 0),
		}, {
			Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.text"},
		}, {
			Op: ir.OpAppend, Payload: ir.CountPayload{Count: 1, Expand: true},
		}, {
			Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	engine, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTestModuleExport(engine, "Main")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != 1 {
		t.Fatalf("result = %#v", result.Values)
	}
	slice, ok := result.Values[0].Data.(*vmSlice)
	if !ok || !slice.ByteBacked || string(slice.ByteBacking[slice.Start:slice.Start+slice.Len]) != "α" {
		t.Fatalf("expanded bytes = %#v", result.Values[0])
	}
}

func TestVMSliceStoreAndClearPreserveElementMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear bool
		want  string
	}{
		{name: "store", want: "Ada"},
		{name: "clear", clear: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
				{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
				{ID: "c.name", Type: testType("String"), Value: json.RawMessage(`"Ada"`)},
			}
			instructions := []ir.Instruction{
				{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.one"}},
				{Op: ir.OpMakeSlice, Payload: testMakeSlicePayload("Slice<User>")},
				{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "local.users"}},
				{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "local.users"}},
				{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.zero"}},
				{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.name"}},
				{Op: ir.OpMakeStruct, Payload: testStructPayload("User", "Name")},
				{Op: ir.OpStoreIndex},
			}
			operands := [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}, {nil, {1}}, {nil, {0}}, {nil, {2}}, {{2}, {3}}, {{1, 0, 3}, nil}}
			if tc.clear {
				operands = append(operands, [2][]uint32{nil, {1}}, [2][]uint32{{1}, nil})
				instructions = append(instructions,
					ir.Instruction{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "local.users"}},
					ir.Instruction{Op: ir.OpClear},
				)
			}
			instructions = append(instructions,
				ir.Instruction{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "local.users"}},
				ir.Instruction{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.zero"}},
				ir.Instruction{Op: ir.OpLoadIndex},
				ir.Instruction{Op: ir.OpLoadField, Payload: ir.FieldPayload{Field: "Name"}},
				ir.Instruction{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
			)
			artifact.Functions = []ir.Function{{
				ID:        "fn.main",
				Signature: testSignature("function() String"),
				Locals:    []ir.Local{{ID: "local.users", Type: testType("Slice<User>")}},
				Code:      testSlotCode([]string{"Int64", "Slice<User>", "String", "User"}, instructions, append(operands, [2][]uint32{nil, {1}}, [2][]uint32{nil, {0}}, [2][]uint32{{1, 0}, {3}}, [2][]uint32{{3}, {2}}, [2][]uint32{{2}, nil})),
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
			requireValues(t, result.Values, newVMValue("String", tc.want))
		})
	}
}

func TestVMCopySliceUsesElementTypeMetadata(t *testing.T) {
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
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
		{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "c.name", Type: testType("String"), Value: json.RawMessage(`"Ada"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Locals: []ir.Local{
			{ID: "local.dst", Type: testType("Slice<User>")},
			{ID: "local.src", Type: testType("Slice<User>")},
		},
		Code: testSlotCode([]string{"Int64", "Slice<User>", "String", "User", "Slice<User>", "Int"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpMakeSlice,
			Payload: testMakeSlicePayload("Slice<User>"),
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.dst"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpMakeSlice,
			Payload: testMakeSlicePayload("Slice<User>"),
		}, {
			Op:      ir.OpStoreLocal,
			Payload: ir.LocalPayload{Local: "local.src"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.src"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.zero"},
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
			Payload: ir.LocalPayload{Local: "local.dst"},
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.src"},
		}, {
			Op: ir.OpCopy,
		}, {
			Op: ir.OpPop,
		}, {
			Op:      ir.OpLoadLocal,
			Payload: ir.LocalPayload{Local: "local.dst"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.zero"},
		}, {
			Op: ir.OpLoadIndex,
		}, {
			Op:      ir.OpLoadField,
			Payload: ir.FieldPayload{Field: "Name"},
		}, {
			Op:      ir.OpReturn,
			Payload: ir.ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}, {nil, {0}}, {{0}, {1}}, {{1}, nil}, {nil, {1}}, {nil, {0}}, {nil, {2}}, {{2}, {3}}, {{1, 0, 3}, nil}, {nil, {1}}, {nil, {4}}, {{1, 4}, {5}}, {{5}, nil}, {nil, {1}}, {nil, {0}}, {{1, 0}, {3}}, {{3}, {2}}, {{2}, nil}}),
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

func TestVMCopySliceRejectsWrongElementType(t *testing.T) {
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
		{ID: "c.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
		{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int64", "Slice<User>", "String", "Slice<String>", "Int"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpMakeSlice,
			Payload: testMakeSlicePayload("Slice<User>"),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op:      ir.OpMakeSequence,
			Payload: testArrayPayload("Slice<String>", 1),
		}, {
			Op: ir.OpCopy,
		}, {
			Op: ir.OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {nil, {2}}, {{2}, {3}}, {{1, 3}, {4}}, {{4}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	_, err = runTestModuleExport(vm, "Main")
	if err == nil || !strings.Contains(err.Error(), "copy element 0") || !strings.Contains(err.Error(), "String is not example/module.User") {
		t.Fatalf("expected copy element type error, got %v", err)
	}
}
