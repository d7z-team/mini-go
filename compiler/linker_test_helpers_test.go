package compiler

import (
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func linkExecutionImage(request linkRequest) (ir.ExecutionImage, error) {
	image, _, err := linkProgram(request)
	return image, err
}

func linkerTestCode(slotTypes []types.TypeRef, instructions []ir.Instruction, operands []ir.RegisterFlow) *ir.MicroCode {
	if len(instructions) != len(operands) {
		panic("linker fixture instruction/operand count mismatch")
	}
	code := &ir.MicroCode{Types: slotTypes, Operands: operands}
	for pc, instruction := range instructions {
		descriptor, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			panic(err)
		}
		code.Instructions = append(code.Instructions, ir.MicroInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc)})
	}
	return code
}
