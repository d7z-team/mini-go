package compiler

import (
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func linkerTestCode(slotTypes []types.TypeRef, instructions []ir.Instruction, operands []ir.SlotOperands) *ir.SlotCode {
	if len(instructions) != len(operands) {
		panic("linker fixture instruction/operand count mismatch")
	}
	code := &ir.SlotCode{Types: slotTypes, Operands: operands}
	for pc, instruction := range instructions {
		descriptor, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			panic(err)
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc)})
	}
	return code
}
