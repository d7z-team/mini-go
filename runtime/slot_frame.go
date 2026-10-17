package runtime

import (
	"encoding/binary"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// integerOperand reads prepared lanes without constructing the general value
// view. Shared/addressable cells keep the synchronized ordinary operand path.
func (f *frame) integerOperand(operand ir.Operand, info types.NumericInfo) (uint64, bool) {
	switch operand.Kind {
	case ir.OperandConstant:
		result := f.module.constantData[operand.Index].result.Load()
		if result != nil && result.err == nil && result.value.scalarKind != 0 {
			return result.value.scalar, true
		}
	case ir.OperandLocal:
		cell := f.localCells[operand.Index]
		if cell.directOwner == f {
			return f.directScalars[cell.directIndex], true
		}
	default:
		index := int(operand.Index)
		if f.tempInitialized[index] && !f.tempHeap[index] {
			offset := f.tempOffsets[index]
			raw := f.tempStorage[offset : offset+f.tempLayouts[index].Stride]
			var bits uint64
			switch len(raw) {
			case 1:
				bits = uint64(raw[0])
			case 2:
				bits = uint64(binary.LittleEndian.Uint16(raw))
			case 4:
				bits = uint64(binary.LittleEndian.Uint32(raw))
			case 8:
				bits = binary.LittleEndian.Uint64(raw)
			default:
				return 0, false
			}
			if info.Kind == types.NumericSigned && info.Bits < 64 {
				bits = uint64(int64(bits<<uint(64-info.Bits)) >> uint(64-info.Bits))
			}
			return bits, true
		}
	}
	return 0, false
}

func (f *frame) beginSlotInstruction(pc int, inst *preparedInstruction) {
	if f.slotPC != pc && f.slotOperands != nil {
		for _, slot := range f.slotOperands.Release {
			f.clearSlotValue(int(slot))
		}
	}
	if f.slotPC != pc {
		for _, slot := range inst.operands.ReleaseBefore {
			f.clearSlotValue(int(slot))
		}
	}
	f.slotPC, f.slotOperands = pc, inst.operands
	f.slotInput, f.slotOutput = len(inst.operands.Inputs), 0
}
