package runtime

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPreparedMinMaxBindsOrderedScalarKind(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		kind minMaxValueKind
	}{
		{name: "signed", typ: "Int32", kind: minMaxSigned},
		{name: "unsigned", typ: "Uint8", kind: minMaxUnsigned},
		{name: "float", typ: "Float32", kind: minMaxFloat},
		{name: "string", typ: "String", kind: minMaxString},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref := testType(test.typ)
			prepared, err := prepareInstruction(ir.Instruction{Op: ir.OpMinMax, Payload: ir.MinMaxPayload{
				Type: ref, Operator: "min", ArgCount: 2,
			}})
			if err != nil {
				t.Fatal(err)
			}
			table := &types.TypeTable{}
			prepared.bindTypes(table, "test")
			if prepared.minMaxKind != test.kind {
				t.Fatalf("prepared kind = %d, want %d", prepared.minMaxKind, test.kind)
			}
		})
	}
}

func TestEvalMinMaxPreservesIntegerWidths(t *testing.T) {
	module := &moduleInstance{}
	int8Type := coerceRuntimeType("Int8")
	int8Result, err := module.evalMinMax(minMaxSigned, "max", []vmValue{
		newVMValue(int8Type, int8(-100)), newVMValue(int8Type, int8(100)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := int8Result.signedValue(); !ok || value != 100 || !int8Result.Type.Equal(int8Type) {
		t.Fatalf("Int8 result = %#v, want typed 100", int8Result)
	}

	uint8Type := coerceRuntimeType("Uint8")
	uint8Result, err := module.evalMinMax(minMaxUnsigned, "min", []vmValue{
		newVMValue(uint8Type, uint8(250)), newVMValue(uint8Type, uint8(3)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := uint8Result.unsignedValue(); !ok || value != 3 || !uint8Result.Type.Equal(uint8Type) {
		t.Fatalf("Uint8 result = %#v, want typed 3", uint8Result)
	}
}

func TestEvalMinMaxFloatNaNAndSignedZero(t *testing.T) {
	module := &moduleInstance{}
	typ := coerceRuntimeType("Float64")
	negativeZero := newVMValue(typ, math.Copysign(0, -1))
	positiveZero := newVMValue(typ, 0.0)
	minimum, err := module.evalMinMax(minMaxFloat, "min", []vmValue{positiveZero, negativeZero})
	if err != nil {
		t.Fatal(err)
	}
	minimumValue, _ := minimum.floatValue()
	if minimumValue != 0 || !math.Signbit(minimumValue) {
		t.Fatalf("min zero = %v, want -0", minimumValue)
	}
	maximum, err := module.evalMinMax(minMaxFloat, "max", []vmValue{negativeZero, positiveZero})
	if err != nil {
		t.Fatal(err)
	}
	maximumValue, _ := maximum.floatValue()
	if maximumValue != 0 || math.Signbit(maximumValue) {
		t.Fatalf("max zero = %v, want +0", maximumValue)
	}

	firstNaN := newVMValue(typ, math.Float64frombits(0x7ff8000000000001))
	secondNaN := newVMValue(typ, math.Float64frombits(0x7ff8000000000002))
	result, err := module.evalMinMax(minMaxFloat, "min", []vmValue{
		newVMValue(typ, 4.0), firstNaN, secondNaN,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.scalar != firstNaN.scalar || !math.IsNaN(math.Float64frombits(result.scalar)) {
		t.Fatalf("NaN result bits = %#x, want first NaN %#x", result.scalar, firstNaN.scalar)
	}
}

func TestEvalMinMaxStringUsesSourceOrderForEqualValues(t *testing.T) {
	module := &moduleInstance{}
	typ := coerceRuntimeType("String")
	result, err := module.evalMinMax(minMaxString, "max", []vmValue{
		newVMValue(typ, "same"), newVMValue(typ, "same"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Data.(string); !ok || value != "same" {
		t.Fatalf("string result = %#v", result)
	}
}

func TestProgramExecutesPreparedMinMax(t *testing.T) {
	artifact := ir.NewArtifact("minmax/runtime", "main")
	artifact.Constants = []ir.Constant{
		{ID: "small", Type: testType("Int32"), Value: json.RawMessage(`-2`)},
		{ID: "large", Type: testType("Int32"), Value: json.RawMessage(`127`)},
	}
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Int32"),
		Code: testMicroCode([]string{"Int32", "Int32", "Int32"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "small"}},
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "large"}},
			{Op: ir.OpMinMax, Payload: ir.MinMaxPayload{Type: testType("Int32"), Operator: "max", ArgCount: 2}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{
			{nil, {0}},
			{nil, {1}},
			{{0, 1}, {2}},
			{{2}, nil},
		}),
	}}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	program := patchTestProgram(t, artifact, "minmax-runtime")
	result, err := program.RunEntry("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != 1 {
		t.Fatalf("result = %#v", result.Values)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 127 {
		t.Fatalf("result = %d, want 127", value)
	}
}
