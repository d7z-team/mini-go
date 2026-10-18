package emit

import (
	"fmt"
	"sort"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// finalizeMicrocode records the static scalar lane and builds executable-PC
// blocks once. Backends consume this proof instead of reconstructing CFG and
// primitive type information in their dispatch loops.
func finalizeMicrocode(code *ir.MicroCode, function *hir.Function, constants []ir.Constant, table *types.TypeTable) error {
	for pc := range code.Instructions {
		instruction := &code.Instructions[pc]
		operands := code.Operands[instruction.Operands]
		var inputType, outputType types.TypeRef
		if len(operands.Inputs) != 0 {
			var ok bool
			inputType, ok = slotOperandType(code, function, constants, operands.Inputs[0])
			if !ok {
				return fmt.Errorf("instruction %d: unresolved input type", pc)
			}
		}
		if len(operands.Outputs) != 0 {
			output := operands.Outputs[0]
			if output&ir.LocalOutput != 0 {
				outputType = function.Locals[output&^ir.LocalOutput].Type
			} else {
				outputType = code.Types[output]
			}
		}
		if inputType.Valid() {
			if primitive, ok := types.View(table, inputType).Primitive(); ok {
				instruction.Mode = uint16(primitive) & 0xff
			}
		}
		if outputType.Valid() {
			if primitive, ok := types.View(table, outputType).Primitive(); ok {
				instruction.Mode |= uint16(primitive) << 8
			}
		}
		instruction.Cost = instruction.LogicalCost()
	}

	labels := make(map[string]uint32)
	wireToExecutable := make([]uint32, len(code.Instructions)+1)
	var executable uint32
	for pc, instruction := range code.Instructions {
		wireToExecutable[pc] = executable
		if instruction.Op == ir.OpLabel {
			payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
			if err != nil {
				return err
			}
			labels[payload.(ir.LabelPayload).Label] = executable
			continue
		}
		executable++
	}
	wireToExecutable[len(code.Instructions)] = executable
	if executable == 0 {
		code.Blocks = nil
		return nil
	}

	leaders := map[uint32]struct{}{0: {}}
	executableInstructions := make([]ir.MicroInstruction, 0, executable)
	executableOperations := make([]ir.Instruction, 0, executable)
	for pc, instruction := range code.Instructions {
		if instruction.Op == ir.OpLabel {
			continue
		}
		payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		if err != nil {
			return err
		}
		operation := ir.Instruction{Op: instruction.Op, Payload: payload}
		executableInstructions = append(executableInstructions, instruction)
		executableOperations = append(executableOperations, operation)
		next := wireToExecutable[pc] + 1
		switch value := payload.(type) {
		case ir.JumpPayload:
			target, ok := labels[value.Label]
			if !ok {
				return fmt.Errorf("instruction %d: unknown label %q", pc, value.Label)
			}
			leaders[target] = struct{}{}
			if next < executable {
				leaders[next] = struct{}{}
			}
		case ir.CompareBranchPayload:
			target, ok := labels[value.Label]
			if !ok {
				return fmt.Errorf("instruction %d: unknown label %q", pc, value.Label)
			}
			leaders[target] = struct{}{}
			if next < executable {
				leaders[next] = struct{}{}
			}
		case ir.UpdateCompareBranchPayload:
			target, ok := labels[value.Label]
			if !ok {
				return fmt.Errorf("instruction %d: unknown label %q", pc, value.Label)
			}
			leaders[target] = struct{}{}
			if next < executable {
				leaders[next] = struct{}{}
			}
		case ir.TypeDispatchPayload:
			target, ok := labels[value.Default]
			if !ok {
				return fmt.Errorf("instruction %d: unknown default label %q", pc, value.Default)
			}
			leaders[target] = struct{}{}
			for _, candidate := range value.Cases {
				target, ok := labels[candidate.Label]
				if !ok {
					return fmt.Errorf("instruction %d: unknown type case label %q", pc, candidate.Label)
				}
				leaders[target] = struct{}{}
			}
			if next < executable {
				leaders[next] = struct{}{}
			}
		default:
			switch instruction.Op {
			case ir.OpReturn, ir.OpPanic, ir.OpTailCallDirect:
				if next < executable {
					leaders[next] = struct{}{}
				}
			}
		}
	}

	starts := make([]uint32, 0, len(leaders))
	for start := range leaders {
		if start < executable {
			starts = append(starts, start)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	blocks := make([]ir.BasicBlock, len(starts))
	for index, start := range starts {
		end := executable
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		block := ir.BasicBlock{First: start, Count: end - start, Fallthrough: ir.NoBlock}
		for pc := start; pc < end; pc++ {
			block.Cost += executableInstructions[pc].LogicalCost()
		}
		last := executableOperations[end-1].Op
		if index+1 < len(starts) && last != ir.OpJump && last != ir.OpReturn && last != ir.OpPanic && last != ir.OpTailCallDirect && last != ir.OpTypeDispatch {
			block.Fallthrough = uint32(index + 1)
		}
		blocks[index] = block
	}
	code.Blocks = blocks
	return nil
}
