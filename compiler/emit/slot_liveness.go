package emit

import (
	"fmt"
	"math/bits"
	"sort"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type slotBlock struct {
	start, end                  int
	successors                  []int
	use, def, in, out           []uint64
	retained, generated, killed []uint64
}

// allocateSlots conservatively extends intervals across CFG edges, then
// reuses only temporaries of the same type. Addressable cells are not part of
// this allocation domain. Release happens after instruction completion.
func allocateSlots(code *ir.SlotCode) error {
	count := len(code.Instructions)
	if count == 0 {
		return nil
	}
	labels := make(map[string]int)
	leaders := map[int]bool{0: true}
	for pc, instruction := range code.Instructions {
		if instruction.Op == ir.OpLabel {
			payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
			if err != nil {
				return err
			}
			name := payload.(ir.LabelPayload).Label
			if _, duplicate := labels[name]; duplicate {
				return fmt.Errorf("duplicate label %q", name)
			}
			labels[name], leaders[pc] = pc, true
		}
		switch instruction.Op {
		case ir.OpJump, ir.OpJumpIf, ir.OpCompareBranch, ir.OpTypeDispatch, ir.OpReturn, ir.OpPanic, ir.OpTailCallDirect:
			if pc+1 < count {
				leaders[pc+1] = true
			}
		}
	}
	starts := make([]int, 0, len(leaders))
	for pc := range leaders {
		starts = append(starts, pc)
	}
	sort.Ints(starts)
	blocks := make([]slotBlock, len(starts))
	owners := make([]int, count)
	words := (len(code.Types) + 63) / 64
	for i, start := range starts {
		end := count
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		blocks[i] = slotBlock{start: start, end: end, use: make([]uint64, words), def: make([]uint64, words), in: make([]uint64, words), out: make([]uint64, words)}
		block := &blocks[i]
		for pc := start; pc < end; pc++ {
			owners[pc] = i
			descriptor := code.Operands[code.Instructions[pc].Operands]
			for _, input := range descriptor.Inputs {
				if input.Kind != ir.OperandSlot {
					continue
				}
				if int(input.Index) >= len(code.Types) {
					return fmt.Errorf("input slot %d out of range", input.Index)
				}
				word, bit := input.Index/64, uint64(1)<<(input.Index%64)
				block.use[word] |= bit &^ block.def[word]
			}
			for _, output := range descriptor.Outputs {
				if output&ir.LocalOutput != 0 {
					continue
				}
				if int(output) >= len(code.Types) {
					return fmt.Errorf("output slot %d out of range", output)
				}
				block.def[output/64] |= uint64(1) << (output % 64)
			}
		}
	}
	for i := range blocks {
		block := &blocks[i]
		last := code.Instructions[block.end-1]
		payload, err := code.Descriptors.Payload(last.Op, last.Descriptor)
		if err != nil {
			return err
		}
		var targets []string
		fallsThrough := true
		switch last.Op {
		case ir.OpCompareBranch:
			targets = []string{payload.(ir.CompareBranchPayload).Label}
		case ir.OpJump, ir.OpJumpIf:
			targets = []string{payload.(ir.JumpPayload).Label}
			fallsThrough = last.Op == ir.OpJumpIf
		case ir.OpTypeDispatch:
			dispatch := payload.(ir.TypeDispatchPayload)
			targets = append(targets, dispatch.Default)
			for _, match := range dispatch.Cases {
				targets = append(targets, match.Label)
			}
			fallsThrough = false
		case ir.OpReturn, ir.OpPanic, ir.OpTailCallDirect:
			fallsThrough = false
		}
		if fallsThrough && i+1 < len(blocks) {
			block.successors = append(block.successors, i+1)
		}
		for _, target := range targets {
			pc, found := labels[target]
			if !found {
				return fmt.Errorf("branch target %q not found", target)
			}
			block.successors = append(block.successors, owners[pc])
		}
	}
	for changed := true; changed; {
		changed = false
		for i := len(blocks) - 1; i >= 0; i-- {
			block := &blocks[i]
			for word := 0; word < words; word++ {
				var outgoing uint64
				for _, successor := range block.successors {
					outgoing |= blocks[successor].in[word]
				}
				incoming := block.use[word] | (outgoing &^ block.def[word])
				if block.in[word] != incoming || block.out[word] != outgoing {
					changed = true
				}
				block.in[word], block.out[word] = incoming, outgoing
			}
		}
	}
	type interval struct {
		slot        uint32
		first, last int
	}
	intervals := make([]interval, len(code.Types))
	for i := range intervals {
		intervals[i] = interval{slot: uint32(i), first: count, last: -1}
	}
	for pc, instruction := range code.Instructions {
		descriptor := code.Operands[instruction.Operands]
		for _, output := range descriptor.Outputs {
			if output&ir.LocalOutput != 0 {
				continue
			}
			life := &intervals[output]
			life.first, life.last = min(life.first, pc), max(life.last, pc)
		}
		for _, input := range descriptor.Inputs {
			if input.Kind == ir.OperandSlot {
				intervals[input.Index].last = max(intervals[input.Index].last, pc)
			}
		}
	}
	for _, block := range blocks {
		for word, incoming := range block.in {
			outgoing := block.out[word]
			for set := incoming | outgoing; set != 0; set &= set - 1 {
				bit := bits.TrailingZeros64(set)
				slot := word*64 + bit
				if incoming&(uint64(1)<<bit) != 0 {
					intervals[slot].first = min(intervals[slot].first, block.start)
				}
				if outgoing&(uint64(1)<<bit) != 0 {
					intervals[slot].last = max(intervals[slot].last, block.end-1)
				}
			}
		}
	}
	sort.SliceStable(intervals, func(i, j int) bool { return intervals[i].first < intervals[j].first })
	// Endpoints are known in advance. A second sorted sweep releases expired
	// intervals without rescanning every live interval for each definition.
	ending := intervals
	for i := 1; i < len(intervals); i++ {
		if intervals[i-1].last > intervals[i].last {
			ending = append([]interval(nil), intervals...)
			sort.SliceStable(ending, func(i, j int) bool { return ending[i].last < ending[j].last })
			break
		}
	}
	expired := 0
	remap := make([]uint32, len(intervals))
	var physical []types.TypeRef
	free := make(map[types.TypeRef][]uint32)
	for _, life := range intervals {
		if life.last < 0 {
			continue
		}
		if life.first == count {
			return fmt.Errorf("slot %d used without definition", life.slot)
		}
		for expired < len(ending) && ending[expired].last < life.first {
			previous := ending[expired]
			expired++
			if previous.last >= 0 {
				typ := code.Types[previous.slot]
				free[typ] = append(free[typ], remap[previous.slot])
			}
		}
		typ := code.Types[life.slot]
		available := free[typ]
		if len(available) == 0 {
			remap[life.slot] = uint32(len(physical))
			physical = append(physical, typ)
		} else {
			remap[life.slot] = available[len(available)-1]
			free[typ] = available[:len(available)-1]
		}
		block := &blocks[owners[life.last]]
		// A linear interval can end at a loop back edge while its value is
		// still live in the successor. Keep that root until overwritten.
		if life.last != block.end-1 || block.out[life.slot/64]&(uint64(1)<<(life.slot%64)) == 0 {
			descriptor := &code.Operands[code.Instructions[life.last].Operands]
			descriptor.Release = append(descriptor.Release, remap[life.slot])
		}
	}
	for i := range code.Operands {
		descriptor := &code.Operands[i]
		for j := range descriptor.Inputs {
			if descriptor.Inputs[j].Kind == ir.OperandSlot {
				descriptor.Inputs[j].Index = remap[descriptor.Inputs[j].Index]
			}
		}
		for j := range descriptor.Outputs {
			if descriptor.Outputs[j]&ir.LocalOutput != 0 {
				continue
			}
			descriptor.Outputs[j] = remap[descriptor.Outputs[j]]
		}
	}
	// Only a value surviving a predecessor can need an entry release.
	// Propagate possible retained physical slots independently of liveness:
	// this over-approximates paths while accounting for normal last-use release.
	physicalWords := (len(physical) + 63) / 64
	for i := range blocks {
		block := &blocks[i]
		block.retained = make([]uint64, physicalWords)
		block.generated = make([]uint64, physicalWords)
		block.killed = make([]uint64, physicalWords)
		for pc := block.start; pc < block.end; pc++ {
			operands := code.Operands[code.Instructions[pc].Operands]
			for _, output := range operands.Outputs {
				if output&ir.LocalOutput != 0 {
					continue
				}
				word, bit := output/64, uint64(1)<<(output%64)
				block.generated[word] |= bit
				block.killed[word] &^= bit
			}
			for _, released := range operands.Release {
				word, bit := released/64, uint64(1)<<(released%64)
				block.killed[word] |= bit
				block.generated[word] &^= bit
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for i := range blocks {
			block := &blocks[i]
			for _, successor := range block.successors {
				target := &blocks[successor]
				for word := range physicalWords {
					outgoing := block.generated[word] | (block.retained[word] &^ block.killed[word])
					if outgoing&^target.retained[word] != 0 {
						target.retained[word] |= outgoing
						changed = true
					}
				}
			}
		}
	}
	// A branch may skip an interval's linear last use. Clear dead reference
	// slots on block entry as well, so a long-lived loop cannot retain values
	// belonging exclusively to a branch it will no longer take.
	live := make([]bool, len(physical))
	for _, block := range blocks[1:] {
		pc := block.start
		for pc < block.end && code.Instructions[pc].Op == ir.OpLabel {
			pc++
		}
		if pc == block.end {
			continue
		}
		clear(live)
		for word, set := range block.in {
			for set != 0 {
				bit := bits.TrailingZeros64(set)
				live[remap[word*64+bit]] = true
				set &= set - 1
			}
		}
		operands := &code.Operands[code.Instructions[pc].Operands]
		for slot, typ := range physical {
			if live[slot] || typ.Kind == types.Void || block.retained[slot/64]&(uint64(1)<<(slot%64)) == 0 {
				continue
			}
			if typ.Kind == types.Primitive && typ.Primitive != types.PrimitiveString && typ.Primitive != types.PrimitiveFunction && typ.Primitive != types.PrimitiveError {
				continue
			}
			operands.ReleaseBefore = append(operands.ReleaseBefore, uint32(slot))
		}
	}
	code.Types = physical
	return nil
}
