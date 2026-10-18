package bytecode

// testMicroCode supplies the complete operand flow; it does not infer a stack.
func testMicroCode(typeNames []string, instructions []Instruction, operands [][2][]uint32) *MicroCode {
	if len(instructions) != len(operands) {
		panic("slot fixture instruction/operand count mismatch")
	}
	code := &MicroCode{}
	for _, name := range typeNames {
		code.Types = append(code.Types, testType(name))
	}
	for pc, instruction := range instructions {
		descriptor, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			panic(err)
		}
		flow := RegisterFlow{Outputs: operands[pc][1], Release: operands[pc][0]}
		for _, slot := range operands[pc][0] {
			flow.Inputs = append(flow.Inputs, Operand{Kind: OperandSlot, Index: slot})
		}
		code.Instructions = append(code.Instructions, MicroInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc), Cost: 1})
		code.Operands = append(code.Operands, flow)
	}
	return code
}
