package runtime

func (f *frame) beginSlotInstruction(pc int, inst *preparedInstruction) {
	if f.slotPC != pc && f.slotOperands != nil {
		for _, slot := range f.slotOperands.Release {
			f.slotValues[slot] = vmValue{}
		}
	}
	if f.slotPC != pc {
		for _, slot := range inst.operands.ReleaseBefore {
			f.slotValues[slot] = vmValue{}
		}
	}
	f.slotPC, f.slotOperands = pc, inst.operands
	f.slotInput, f.slotOutput = len(inst.operands.Inputs), 0
}
