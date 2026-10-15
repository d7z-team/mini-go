package runtime

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPreparedNumericOperandsPreserveNamedTypesAndArithmeticBoundaries(t *testing.T) {
	for _, test := range []struct {
		kind                  types.PrimitiveKind
		op                    preparedOperator
		left, right, expected any
	}{
		{types.PrimitiveInt8, operatorAdd, int64(127), int64(1), int64(-128)},
		{types.PrimitiveInt8, operatorDiv, int64(-128), int64(-1), int64(-128)},
		{types.PrimitiveInt8, operatorShiftRight, int64(-128), uint64(64), int64(-1)},
		{types.PrimitiveUint8, operatorAdd, uint64(255), uint64(1), uint64(0)},
		{types.PrimitiveUint8, operatorShiftLeft, uint64(255), uint64(64), uint64(0)},
		{types.PrimitiveFloat32, operatorAdd, float64(16777216), float64(1), float64(16777216)},
		{types.PrimitiveInt8, operatorLess, int64(-1), int64(1), true},
		{types.PrimitiveComplex64, operatorAdd, complex(16777216, 1), complex(1, 2), complex(16777216, 3)},
	} {
		t.Run(types.PrimitiveName(test.kind)+test.op.String(), func(t *testing.T) {
			artifact := ir.NewArtifact("sample", "sample")
			node := types.TypeNode{ID: "number", Kind: types.Named, Identity: types.TypeKey{ModulePath: "sample", DeclID: "Number"}, Underlying: types.Builtin(test.kind)}
			if err := artifact.TypeTable.Add(node); err != nil {
				t.Fatal(err)
			}
			typ := runtimeTypeWithTable(types.Ref(node), &artifact.TypeTable)
			left, right := newVMValue(typ, test.left), newVMValue(typ, test.right)
			numeric := [2]types.PrimitiveKind{test.kind, test.kind}
			if test.op == operatorShiftLeft || test.op == operatorShiftRight {
				right = newVMValue("Uint64", test.right)
				numeric[1] = types.PrimitiveUint64
			}
			for _, preparation := range [][2]types.PrimitiveKind{{}, numeric} {
				value, err := (&moduleInstance{}).evalBinary(test.op, left, right, preparation)
				if err != nil {
					t.Fatal(err)
				}
				resultType := typ
				if _, boolean := test.expected.(bool); boolean {
					resultType = boolRuntimeType
				}
				requireValues(t, []vmValue{value}, newVMValue(resultType, test.expected))
			}
		})
	}
}

func TestPreparedNumericFailuresRemainGuestPanics(t *testing.T) {
	for _, test := range []struct {
		op    preparedOperator
		right int64
	}{
		{operatorDiv, 0}, {operatorMod, 0}, {operatorShiftLeft, -1}, {operatorShiftRight, -1},
	} {
		_, err := (&moduleInstance{}).evalBinary(test.op, newVMValue("Int", int64(1)), newVMValue("Int", test.right), [2]types.PrimitiveKind{types.PrimitiveInt, types.PrimitiveInt})
		var panicValue *guestPanic
		if !errors.As(err, &panicValue) {
			t.Fatalf("%s: got %T %v, want guest panic", test.op, err, err)
		}
	}
	value, err := (&moduleInstance{}).evalUnary(operatorSub, newVMValue("Int8", int64(-128)), types.PrimitiveInt8)
	if err != nil {
		t.Fatal(err)
	}
	requireValues(t, []vmValue{value}, newVMValue("Int8", int64(-128)))
}

func TestPrimitiveConversionFastPathPreservesNamedTargetsAndBoundaries(t *testing.T) {
	table := types.NewTable(
		types.TypeNode{
			ID: "type.Small", Kind: types.Named,
			Identity:   types.TypeKey{ModulePath: "sample", DeclID: "Small"},
			Underlying: types.Builtin(types.PrimitiveInt8),
		},
		types.TypeNode{
			ID: "type.Flag", Kind: types.Named,
			Identity:   types.TypeKey{ModulePath: "sample", DeclID: "Flag"},
			Underlying: types.Builtin(types.PrimitiveBool),
		},
		types.TypeNode{
			ID: "type.Text", Kind: types.Named,
			Identity:   types.TypeKey{ModulePath: "sample", DeclID: "Text"},
			Underlying: types.Builtin(types.PrimitiveString),
		},
	)
	small, ok := table.Named(types.TypeKey{ModulePath: "sample", DeclID: "Small"})
	if !ok {
		t.Fatal("missing named integer type")
	}
	flag, ok := table.Named(types.TypeKey{ModulePath: "sample", DeclID: "Flag"})
	if !ok {
		t.Fatal("missing named bool type")
	}
	text, ok := table.Named(types.TypeKey{ModulePath: "sample", DeclID: "Text"})
	if !ok {
		t.Fatal("missing named string type")
	}

	module := &moduleInstance{}
	smallType := runtimeTypeWithTable(types.Ref(small), table)
	flagType := runtimeTypeWithTable(types.Ref(flag), table)
	textType := runtimeTypeWithTable(types.Ref(text), table)
	for _, test := range []struct {
		name   string
		value  vmValue
		target vmType
		want   vmValue
	}{
		{
			name:   "signed truncation",
			value:  newVMValue("Int64", int64(257)),
			target: smallType,
			want:   newVMValue(smallType, int64(1)),
		},
		{
			name:   "named bool",
			value:  newVMValue("Bool", true),
			target: flagType,
			want:   newVMValue(flagType, true),
		},
		{
			name:   "named string",
			value:  newVMValue("String", "ready"),
			target: textType,
			want:   newVMValue(textType, "ready"),
		},
		{
			name:   "integer to named string",
			value:  newVMValue("Int", int64('界')),
			target: textType,
			want:   newVMValue(textType, "界"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := module.convertValue(test.value, test.target)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			requireValues(t, []vmValue{got}, test.want)
		})
	}
}

func TestPrimitiveConversionFastPathRejectsNonNumericFloatToString(t *testing.T) {
	textType := coerceRuntimeType("String")
	if _, err := (&moduleInstance{}).convertValue(newVMValue("Float64", 1.5), textType); err == nil {
		t.Fatal("float to string conversion unexpectedly succeeded")
	}
}

func TestNegativeShiftIsGuestPanic(t *testing.T) {
	_, err := (&moduleInstance{}).evalBinary(operatorShiftLeft, newVMValue("Int", int64(1)), newVMValue("Int", int64(-1)), [2]types.PrimitiveKind{})
	var panicValue *guestPanic
	if !errors.As(err, &panicValue) {
		t.Fatalf("negative shift error = %T %v, want guest panic", err, err)
	}
}

func TestParseConstantComplex128RequiresCanonicalObject(t *testing.T) {
	value, err := parseConstantComplex128(json.RawMessage(`{"real":1.5,"imag":-2}`))
	if err != nil || value != complex(1.5, -2) {
		t.Fatalf("parse canonical complex = %v, %v", value, err)
	}
	for _, raw := range []string{
		`"1+2i"`,
		`{}`,
		`{"real":1}`,
		`{"real":1,"imag":2,"extra":3}`,
		`{"real":"1","imag":2}`,
		`{"real":null,"imag":2}`,
	} {
		if _, err := parseConstantComplex128(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted malformed complex constant %s", raw)
		}
	}
}
