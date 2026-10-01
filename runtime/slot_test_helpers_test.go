package runtime

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// testSlotCode assembles mnemonic descriptors with explicit temporary inputs
// and outputs. Consumed inputs are released at the next instruction boundary.
func testSlotCode(typeNames []string, instructions []ir.Instruction, operands [][2][]uint32) *ir.SlotCode {
	if len(instructions) != len(operands) {
		panic("slot fixture instruction/operand count mismatch")
	}
	code := &ir.SlotCode{}
	for _, name := range typeNames {
		code.Types = append(code.Types, testType(name))
	}
	appendTestSlotCode(code, instructions, operands)
	return code
}

func appendTestSlotCode(code *ir.SlotCode, instructions []ir.Instruction, operands [][2][]uint32) {
	if len(instructions) != len(operands) {
		panic("slot fixture instruction/operand count mismatch")
	}
	for pc, instruction := range instructions {
		descriptor, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			panic(err)
		}
		flow := ir.SlotOperands{Outputs: operands[pc][1], Release: operands[pc][0]}
		for _, slot := range operands[pc][0] {
			flow.Inputs = append(flow.Inputs, ir.Operand{Kind: ir.OperandSlot, Index: slot})
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(len(code.Operands))})
		code.Operands = append(code.Operands, flow)
	}
}

// insertTestDelay preserves the charged zero/pop pairs used by scheduling tests.
func insertTestDelay(code *ir.SlotCode, pc, pairs int) {
	slot := uint32(len(code.Types))
	code.Types = append(code.Types, testType("Bool"))
	tail := append([]ir.SlotInstruction(nil), code.Instructions[pc:]...)
	code.Instructions = code.Instructions[:pc]
	for range pairs {
		appendTestSlotCode(code, []ir.Instruction{{Op: ir.OpZero, Payload: testTypePayload("Bool")}, {Op: ir.OpPop}}, [][2][]uint32{{nil, {slot}}, {{slot}, nil}})
	}
	code.Instructions = append(code.Instructions, tail...)
}

// slotLoopArtifact executes three charged instructions per iteration and one
// return. It shares the same canonical program between budget tests and timing.
func slotLoopArtifact(t testing.TB, iterations int) ir.Artifact {
	t.Helper()
	integer := types.Builtin(types.PrimitiveInt)
	artifact := ir.NewArtifact("slot/loop", "main")
	artifact.Constants = []ir.Constant{
		{ID: "one", Type: integer, Value: json.RawMessage(`1`)},
		{ID: "end", Type: integer, Value: json.RawMessage(strconv.Itoa(iterations))},
	}
	code := ir.SlotCode{Types: []types.TypeRef{integer}, Operands: []ir.SlotOperands{
		{},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant}}, Outputs: []uint32{0}},
		{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant, Index: 1}}},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}}},
	}}
	for pc, instruction := range []ir.Instruction{
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "loop"}},
		{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "+"}},
		{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "counter"}},
		{Op: ir.OpCompareBranch, Payload: ir.CompareBranchPayload{Operator: "<", Type: integer, Label: "loop", When: true}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
	} {
		descriptor, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			t.Fatal(err)
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc)})
	}
	artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: types.FunctionSignature{Results: []types.TypeRef{integer}}, Locals: []ir.Local{{ID: "counter", Type: integer}}, Code: &code}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.entry"}}
	return artifact
}
