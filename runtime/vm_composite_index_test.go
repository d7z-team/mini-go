package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestVMMakeSliceUsesNamedStructTypeMetadata(t *testing.T) {
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
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() String"),
		Code: testSlotCode([]string{"Int64", "Slice<User>", "User", "String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpMakeSlice,
			Payload: testMakeSlicePayload("Slice<User>"),
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
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {nil, {0}}, {{1, 0}, {2}}, {{2}, {3}}, {{3}, nil}}),
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

func TestLoaderSetIndexSliceRejectsWrongElementType(t *testing.T) {
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
		{ID: "c.bad", Type: testType("String"), Value: json.RawMessage(`"bad"`)},
	}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int64", "Slice<User>", "String"}, []ir.Instruction{{
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.one"},
		}, {
			Op:      ir.OpMakeSlice,
			Payload: testMakeSlicePayload("Slice<User>"),
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.zero"},
		}, {
			Op:      ir.OpConst,
			Payload: ir.ConstPayload{Constant: "c.bad"},
		}, {
			Op: ir.OpStoreIndex,
		}}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {nil, {0}}, {nil, {2}}, {{1, 0, 2}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	_, err := loadTestEngine(artifact)
	if err == nil || !strings.Contains(err.Error(), "String is not assignable to example/module.User") {
		t.Fatalf("expected slot validation error, got %v", err)
	}
}
