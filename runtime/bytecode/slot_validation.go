package bytecode

import (
	"errors"
	"fmt"
	"strconv"
)

// ValidateSlotCode checks descriptor references, operand arity and definite
// initialization across every reachable edge. Slots contain typed values;
// clearing a slot ends its initialized lifetime until its next definition.
func ValidateSlotCode(code *SlotCode, constants []Constant, locals ...Local) error {
	return validateSlotCode(code, constants, locals, nil)
}

// validated contains payloads already checked by validateFunctionBody. Keeping
// that proof local avoids a second descriptor decode and payload validation.
func validateSlotCode(code *SlotCode, constants []Constant, locals []Local, validated []Instruction) error {
	if code == nil {
		return errors.New("missing slot code")
	}
	labels := make(map[string]int)
	for pc, instruction := range code.Instructions {
		if uint64(instruction.Operands) >= uint64(len(code.Operands)) {
			return fmt.Errorf("instruction %d: operands out of range", pc)
		}
		var inst Instruction
		if validated != nil {
			inst = validated[pc]
		} else {
			payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
			if err != nil {
				return fmt.Errorf("instruction %d: %w", pc, err)
			}
			inst = Instruction{Op: instruction.Op, Payload: payload}
			if err := validateInstruction("", &inst); err != nil {
				path := "instructions[" + strconv.Itoa(pc) + "]"
				return prependValidationPath(path, err)
			}
		}
		payload := inst.Payload
		inputCount, outputCount, err := instructionArity(&inst)
		if err != nil {
			return err
		}
		operands := code.Operands[instruction.Operands]
		if len(operands.Inputs) != inputCount || len(operands.Outputs) != outputCount {
			return fmt.Errorf("instruction %d: operand arity mismatch", pc)
		}
		for _, input := range operands.Inputs {
			switch input.Kind {
			case OperandSlot:
				if uint64(input.Index) >= uint64(len(code.Types)) {
					return fmt.Errorf("instruction %d: input slot out of range", pc)
				}
			case OperandConstant:
				if uint64(input.Index) >= uint64(len(constants)) {
					return fmt.Errorf("instruction %d: constant out of range", pc)
				}
				if constants[input.Index].Untyped {
					return fmt.Errorf("instruction %d: untyped constant is not an execution operand", pc)
				}
			case OperandLocal:
				if uint64(input.Index) >= uint64(len(locals)) {
					return fmt.Errorf("instruction %d: local operand out of range", pc)
				}
			default:
				return fmt.Errorf("instruction %d: unknown operand kind", pc)
			}
		}
		var outputs map[uint32]bool
		if len(operands.Outputs) > 1 {
			outputs = make(map[uint32]bool, len(operands.Outputs))
		}
		for _, slot := range operands.Outputs {
			if slot&LocalOutput != 0 {
				if !HasDirectOutput(instruction.Op) || len(operands.Outputs) != 1 || uint64(slot&^LocalOutput) >= uint64(len(locals)) {
					return fmt.Errorf("instruction %d: invalid direct local output", pc)
				}
			} else if uint64(slot) >= uint64(len(code.Types)) {
				return fmt.Errorf("instruction %d: output slot out of range", pc)
			}
			if outputs[slot] {
				return fmt.Errorf("instruction %d: duplicate output slot %d", pc, slot)
			}
			for _, input := range operands.Inputs {
				if input.Kind == OperandSlot && input.Index == slot {
					return fmt.Errorf("instruction %d: output aliases input slot %d", pc, slot)
				}
			}
			if outputs != nil {
				outputs[slot] = true
			}
		}
		var releases map[uint32]bool
		if len(operands.Release) > 1 {
			releases = make(map[uint32]bool, len(operands.Release))
		}
		for _, slot := range operands.Release {
			if uint64(slot) >= uint64(len(code.Types)) {
				return fmt.Errorf("instruction %d: release slot out of range", pc)
			}
			if releases[slot] {
				return fmt.Errorf("instruction %d: duplicate release slot %d", pc, slot)
			}
			if releases != nil {
				releases[slot] = true
			}
		}
		if instruction.Op == OpLabel {
			if len(operands.Release) != 0 || len(operands.ReleaseBefore) != 0 {
				return fmt.Errorf("instruction %d: label cannot release slots", pc)
			}
			label := payload.(LabelPayload).Label
			if _, found := labels[label]; found {
				return fmt.Errorf("duplicate label %q", label)
			}
			labels[label] = pc
		}
		clear(releases)
		if len(operands.ReleaseBefore) > 1 && releases == nil {
			releases = make(map[uint32]bool, len(operands.ReleaseBefore))
		}
		for _, slot := range operands.ReleaseBefore {
			if uint64(slot) >= uint64(len(code.Types)) {
				return fmt.Errorf("instruction %d: entry release slot out of range", pc)
			}
			if releases[slot] {
				return fmt.Errorf("instruction %d: duplicate entry release slot %d", pc, slot)
			}
			if releases != nil {
				releases[slot] = true
			}
		}
	}
	for i, typ := range code.Types {
		if !typ.Valid() {
			return fmt.Errorf("slot %d: invalid type", i)
		}
	}
	successors := make([][]int, len(code.Instructions))
	leaders := make([]bool, len(code.Instructions))
	for pc, instruction := range code.Instructions {
		var payload Payload
		if validated != nil {
			payload = validated[pc].Payload
		} else {
			payload, _ = code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		}
		var targets []string
		fallsThrough := true
		switch instruction.Op {
		case OpCompareBranch:
			targets = append(targets, payload.(CompareBranchPayload).Label)
		case OpJump, OpJumpIf:
			targets = append(targets, payload.(JumpPayload).Label)
			fallsThrough = instruction.Op == OpJumpIf
		case OpTypeDispatch:
			dispatch := payload.(TypeDispatchPayload)
			targets = append(targets, dispatch.Default)
			for _, match := range dispatch.Cases {
				targets = append(targets, match.Label)
			}
			fallsThrough = false
		case OpPanic, OpReturn, OpTailCallDirect:
			fallsThrough = false
		}
		if fallsThrough && pc+1 < len(code.Instructions) {
			successors[pc] = append(successors[pc], pc+1)
		}
		if (len(targets) != 0 || !fallsThrough) && pc+1 < len(leaders) {
			leaders[pc+1] = true
		}
		for _, label := range targets {
			target, found := labels[label]
			if !found {
				return fmt.Errorf("instruction %d: unknown label %q", pc, label)
			}
			successors[pc] = append(successors[pc], target)
			leaders[target] = true
		}
	}
	if len(code.Instructions) == 0 {
		return nil
	}
	words := (len(code.Types) + 63) / 64
	// Bound analysis independently of execution, including hostile dense CFGs.
	// This is 32 MiB of bitsets; normal allocated frames need a few words.
	if uint64(words)*uint64(len(code.Instructions)) > 4<<20 {
		return errors.New("slot initialization analysis exceeds limit")
	}
	states := make([][]uint64, len(code.Instructions))
	states[0] = make([]uint64, words)
	seen := make([]bool, len(states))
	seen[0] = true
	queued := make([]bool, len(states))
	queued[0] = true
	queue := []int{0}
	state := make([]uint64, words)
	for len(queue) != 0 {
		pc := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		queued[pc] = false
		copy(state, states[pc])
		// Only block entries need a retained state. Instructions on a linear
		// path update the same scratch bitset without copying it at every PC.
		for {
			operands := code.Operands[code.Instructions[pc].Operands]
			for _, slot := range operands.ReleaseBefore {
				state[slot/64] &^= uint64(1) << (slot % 64)
			}
			for _, input := range operands.Inputs {
				if input.Kind == OperandSlot && state[input.Index/64]&(uint64(1)<<(input.Index%64)) == 0 {
					return fmt.Errorf("instruction %d: slot %d may be uninitialized", pc, input.Index)
				}
			}
			for _, output := range operands.Outputs {
				if output&LocalOutput != 0 {
					continue
				}
				state[output/64] |= uint64(1) << (output % 64)
			}
			for _, slot := range operands.Release {
				state[slot/64] &^= uint64(1) << (slot % 64)
			}
			if len(successors[pc]) != 1 || successors[pc][0] != pc+1 || leaders[pc+1] {
				break
			}
			pc++
		}
		for _, target := range successors[pc] {
			changed := !seen[target]
			if !seen[target] {
				states[target], seen[target] = append([]uint64(nil), state...), true
			} else {
				for word, outgoing := range state {
					shared := states[target][word] & outgoing
					if shared != states[target][word] {
						changed = true
						states[target][word] = shared
					}
				}
			}
			if changed && !queued[target] {
				queued[target] = true
				queue = append(queue, target)
			}
		}
	}
	return nil
}
