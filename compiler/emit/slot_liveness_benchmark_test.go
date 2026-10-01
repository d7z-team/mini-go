package emit

import (
	"strconv"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// Overlap exercises large constructors; short lifetimes exercise normal
// sequential expressions. Setup is included equally in both algorithms.
func BenchmarkSlotLiveness(b *testing.B) {
	for _, overlap := range []bool{false, true} {
		for _, count := range []int{128, 512, 2048} {
			b.Run(strconv.FormatBool(overlap)+"/"+strconv.Itoa(count), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					code := ir.SlotCode{}
					code.Descriptors.Type = []ir.TypePayload{{Type: types.Builtin(types.PrimitiveInt)}}
					code.Descriptors.Return = []ir.ReturnPayload{{ResultCount: count}}
					inputs := make([]ir.Operand, count)
					for i := 0; i < count; i++ {
						code.Types = append(code.Types, types.Builtin(types.PrimitiveInt))
						code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: ir.OpZero, Operands: uint32(len(code.Operands))})
						code.Operands = append(code.Operands, ir.SlotOperands{Outputs: []uint32{uint32(i)}})
						inputs[i] = ir.Operand{Kind: ir.OperandSlot, Index: uint32(i)}
						if !overlap {
							code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: ir.OpPop, Operands: uint32(len(code.Operands))})
							code.Operands = append(code.Operands, ir.SlotOperands{Inputs: []ir.Operand{inputs[i]}})
						}
					}
					if !overlap {
						inputs = nil
						code.Descriptors.Return[0].ResultCount = 0
					}
					code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: ir.OpReturn, Operands: uint32(len(code.Operands))})
					code.Operands = append(code.Operands, ir.SlotOperands{Inputs: inputs})
					if err := allocateSlots(&code); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
