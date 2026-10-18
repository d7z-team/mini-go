package runtime

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPreparedInstructionBindsOperator(t *testing.T) {
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "+"}})
	if err != nil {
		t.Fatal(err)
	}
	if instruction.op != preparedBinary || instruction.operator != operatorAdd {
		t.Fatalf("prepared instruction = %#v", instruction)
	}
	if instruction.control {
		t.Fatal("ordinary binary instruction was classified as scheduler control")
	}
}

func TestPreparedInstructionBindsRuntimeTypeAndStructSchema(t *testing.T) {
	table := &types.TypeTable{}
	ref, err := types.NewParser("example/module", table).Parse("struct{Value:Int}")
	if err != nil {
		t.Fatal(err)
	}
	payload := ir.MakeStructPayload{Type: ref, Fields: []string{"Value"}}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpMakeStruct, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	instruction.bindTypes(table, "example/module")
	if got := instruction.runtimeType.String(); got != "struct{Value:Int}" {
		t.Fatalf("prepared type = %q", got)
	}
	if instruction.structSchema == nil || len(instruction.structSchema.fields) != 1 || instruction.structSchema.fields[0].Name != "Value" {
		t.Fatalf("prepared schema = %#v", instruction.structSchema)
	}
}

func TestPreparedConversionBindsScalarMetadata(t *testing.T) {
	table := &types.TypeTable{}
	ref, err := types.NewParser("example/module", table).Parse("Int32")
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpConvert, Payload: ir.TypePayload{Type: ref}})
	if err != nil {
		t.Fatal(err)
	}
	instruction.bindTypes(table, "example/module")
	if !instruction.conversion.numericOK || instruction.conversion.numeric.Kind != types.NumericSigned || instruction.conversion.numeric.Bits != 32 {
		t.Fatalf("prepared conversion metadata = %#v", instruction.conversion)
	}
	if !instruction.conversion.scalarTarget() {
		t.Fatal("numeric conversion was not classified as scalar")
	}
	frame := &frame{
		module:       &moduleInstance{},
		slotValues:   []vmValue{newVMValue("Int64", int64(257))},
		slotOperands: &ir.RegisterFlow{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}, Outputs: []uint32{0}},
		slotInput:    1,
	}
	if err := (&vm{}).executeInstruction(nil, frame, &instruction); err != nil {
		t.Fatalf("prepared conversion: %v", err)
	}
	converted := frame.slotValues[0]
	if !converted.Type.Equal(instruction.runtimeType) || converted.materializedData() != int64(257) {
		t.Fatalf("prepared conversion result = %#v", converted)
	}
}

func TestPreparedConversionPreservesNamedScalarAndAggregateSemantics(t *testing.T) {
	table := &types.TypeTable{}
	parser := types.NewParser("example/module", table)
	int32Ref, err := parser.Parse("Int32")
	if err != nil {
		t.Fatal(err)
	}
	uint8Ref, err := parser.Parse("Uint8")
	if err != nil {
		t.Fatal(err)
	}
	boolRef, err := parser.Parse("Bool")
	if err != nil {
		t.Fatal(err)
	}
	stringRef, err := parser.Parse("String")
	if err != nil {
		t.Fatal(err)
	}
	sliceRef, err := parser.Parse("Slice<Int32>")
	if err != nil {
		t.Fatal(err)
	}
	namedInt, err := parser.Define("Count", int32Ref, false)
	if err != nil {
		t.Fatal(err)
	}
	namedByte, err := parser.Define("Byte", uint8Ref, false)
	if err != nil {
		t.Fatal(err)
	}
	namedBool, err := parser.Define("Flag", boolRef, false)
	if err != nil {
		t.Fatal(err)
	}
	namedString, err := parser.Define("Label", stringRef, false)
	if err != nil {
		t.Fatal(err)
	}
	namedRunes, err := parser.Define("Runes", sliceRef, false)
	if err != nil {
		t.Fatal(err)
	}

	convert := func(target types.TypeRef, input vmValue) (vmValue, error) {
		instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpConvert, Payload: ir.TypePayload{Type: target}})
		if err != nil {
			return vmValue{}, err
		}
		instruction.bindTypes(table, "example/module")
		frame := &frame{
			module:       &moduleInstance{},
			slotValues:   []vmValue{input},
			slotOperands: &ir.RegisterFlow{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}, Outputs: []uint32{0}},
			slotInput:    1,
		}
		if err := (&vm{}).executeInstruction(nil, frame, &instruction); err != nil {
			return vmValue{}, err
		}
		return frame.slotValues[0], nil
	}

	tests := []struct {
		name   string
		target types.TypeRef
		input  vmValue
		check  func(t *testing.T, value vmValue)
	}{
		{
			name:   "named integer",
			target: namedInt,
			input:  newVMValue("Int64", int64(257)),
			check: func(t *testing.T, value vmValue) {
				if value.Type.Ref != namedInt || value.materializedData() != int64(257) {
					t.Fatalf("converted named integer = %#v", value)
				}
			},
		},
		{
			name:   "named byte truncation",
			target: namedByte,
			input:  newVMValue("Int64", int64(-1)),
			check: func(t *testing.T, value vmValue) {
				if value.Type.Ref != namedByte || value.materializedData() != uint64(255) {
					t.Fatalf("converted named byte = %#v", value)
				}
			},
		},
		{
			name:   "named boolean",
			target: namedBool,
			input:  newBoolValue(true),
			check: func(t *testing.T, value vmValue) {
				if value.Type.Ref != namedBool || value.materializedData() != true {
					t.Fatalf("converted named boolean = %#v", value)
				}
			},
		},
		{
			name:   "named string from rune",
			target: namedString,
			input:  newVMValue("Int32", int64('A')),
			check: func(t *testing.T, value vmValue) {
				if value.Type.Ref != namedString || value.materializedData() != "A" {
					t.Fatalf("converted named string = %#v", value)
				}
			},
		},
		{
			name:   "named rune slice fallback",
			target: namedRunes,
			input:  newVMValue("String", "a尾"),
			check: func(t *testing.T, value vmValue) {
				values, ok := sliceValues(value)
				if !ok || value.Type.Ref != namedRunes || len(values) != 2 || values[0].materializedData() != int64('a') || values[1].materializedData() != int64('尾') {
					t.Fatalf("converted named rune slice = %#v", value)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := convert(test.target, test.input)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, value)
		})
	}
}

func TestPreparedDirectCallUsesCurrentModuleByDefault(t *testing.T) {
	payload := ir.CallPayload{Function: "fn.target"}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpCallDirect, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	instruction.bindTypes(&types.TypeTable{}, "example/module")
	if instruction.callModule != "example/module" {
		t.Fatalf("prepared call module = %q", instruction.callModule)
	}
	if !instruction.control {
		t.Fatal("direct call was not classified as scheduler control")
	}
}

func TestPreparedTailCallIsSchedulerControl(t *testing.T) {
	payload := ir.CallPayload{Function: "fn.target", ResultCount: 1}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpTailCallDirect, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if instruction.op != preparedTailCallDirect || !instruction.control {
		t.Fatalf("prepared tail call = %#v", instruction)
	}
}
