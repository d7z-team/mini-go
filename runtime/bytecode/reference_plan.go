package bytecode

import (
	"errors"
	"fmt"
	"slices"
)

// FrameReferences describes references at instruction entry. Bit positions are
// locals, then temporaries, then upvalue cells. A runtime intersects each map
// with initialized storage and traces each selected storage location's layout.
// Maps use wire PCs, including labels; loaders apply their existing PC mapping.
// Operation temporaries and suspended continuations are separate runtime roots.
type FrameReferences struct {
	Cells []bool
	Maps  [][]uint64
	AtPC  []uint32
}

type referenceEdge struct {
	pc   int
	kill int
}

// PrepareFrameReferences derives roots from final operands and descriptors.
// localReferences and temporaryReferences describe the runtime's actual storage
// layouts, including boxes for aggregates that do not fit inline storage.
// Addressed locals and upvalues are cells even when their contents are scalar.
// storageCells additionally identifies backend-owned storage records: writes
// need their existing identity even when the old value is otherwise dead.
func PrepareFrameReferences(fn *Function, localReferences, temporaryReferences, storageCells []bool) (FrameReferences, error) {
	if fn == nil || fn.Code == nil || len(localReferences) != len(fn.Locals) || len(temporaryReferences) != len(fn.Code.Types) {
		return FrameReferences{}, errors.New("reference plan storage does not match function")
	}
	if storageCells != nil && len(storageCells) != len(fn.Locals) {
		return FrameReferences{}, errors.New("reference plan cells do not match function")
	}
	operations, err := fn.Operations()
	if err != nil {
		return FrameReferences{}, err
	}
	count := len(operations)
	localCount, temporaryCount := len(fn.Locals), len(fn.Code.Types)
	storageCount := localCount + temporaryCount + len(fn.Upvalues)
	words := (storageCount + 63) / 64
	plan := FrameReferences{Cells: make([]bool, localCount), AtPC: make([]uint32, count+1)}
	locals, upvalues, labels := make(map[string]int, localCount), make(map[string]int, len(fn.Upvalues)), make(map[string]int)
	for index, local := range fn.Locals {
		locals[local.ID] = index
	}
	for index, upvalue := range fn.Upvalues {
		upvalues[upvalue.ID] = localCount + temporaryCount + index
	}
	lookupLocal := func(name string) int {
		index, found := locals[name]
		if !found {
			err = fmt.Errorf("reference plan has unknown local %q", name)
			return -1
		}
		return index
	}
	lookupUpvalue := func(name string) int {
		index, found := upvalues[name]
		if !found {
			err = fmt.Errorf("reference plan has unknown upvalue %q", name)
			return -1
		}
		return index
	}
	markCell := func(address AddressPayload) {
		if address.Kind == "local" {
			if index := lookupLocal(address.Local); index >= 0 {
				plan.Cells[index] = true
			}
		}
	}
	for pc, operation := range operations {
		switch payload := operation.Payload.(type) {
		case AddressPayload:
			markCell(payload)
		case ClosurePayload:
			for _, capture := range payload.Captures {
				markCell(capture)
			}
		case LabelPayload:
			if _, exists := labels[payload.Label]; exists {
				return FrameReferences{}, fmt.Errorf("reference plan has duplicate label %q", payload.Label)
			}
			labels[payload.Label] = pc
		}
	}
	if err != nil {
		return FrameReferences{}, err
	}
	containsReference := make([]bool, storageCount)
	cellIdentity := slices.Clone(plan.Cells)
	for index, cell := range storageCells {
		cellIdentity[index] = cellIdentity[index] || cell
	}
	for index, hasReferences := range localReferences {
		containsReference[index] = hasReferences || cellIdentity[index]
	}
	copy(containsReference[localCount:], temporaryReferences)
	for index := localCount + temporaryCount; index < storageCount; index++ {
		containsReference[index] = true
	}
	// Fixed-width working storage is discarded after bitmap interning. Nothing
	// in this dataflow pass is retained by the execution image or a global cache.
	uses, defs, incoming := make([]uint64, count*words), make([]uint64, count*words), make([]uint64, (count+1)*words)
	edges := make([][]referenceEdge, count)
	set := func(bitmap []uint64, index int) {
		if index >= 0 && index < storageCount && containsReference[index] {
			bitmap[index/64] |= uint64(1) << (index % 64)
		}
	}
	resultRoots := make([]uint64, words)
	for _, result := range fn.ResultLocals {
		set(resultRoots, lookupLocal(result))
	}
	copy(incoming[count*words:], resultRoots)
	for pc, operation := range operations {
		use, def := uses[pc*words:(pc+1)*words], defs[pc*words:(pc+1)*words]
		// Any instruction can fail and unwind through a recovering defer. Named
		// results remain observable through that implicit completion path.
		copy(use, resultRoots)
		instruction := fn.Code.Instructions[pc]
		if int(instruction.Operands) >= len(fn.Code.Operands) {
			return FrameReferences{}, fmt.Errorf("reference plan operands out of range at %d", pc)
		}
		operands := fn.Code.Operands[instruction.Operands]
		for _, input := range operands.Inputs {
			switch input.Kind {
			case OperandSlot:
				if int(input.Index) >= temporaryCount {
					return FrameReferences{}, fmt.Errorf("reference plan temporary out of range at %d", pc)
				}
				set(use, localCount+int(input.Index))
			case OperandLocal:
				if int(input.Index) >= localCount {
					return FrameReferences{}, fmt.Errorf("reference plan local out of range at %d", pc)
				}
				set(use, int(input.Index))
			}
		}
		for _, output := range operands.Outputs {
			if output&LocalOutput != 0 {
				index := int(output &^ LocalOutput)
				if index >= localCount {
					return FrameReferences{}, fmt.Errorf("reference plan output local out of range at %d", pc)
				}
				set(def, index)
				if cellIdentity[index] {
					set(use, index)
				}
			} else {
				if int(output) >= temporaryCount {
					return FrameReferences{}, fmt.Errorf("reference plan output temporary out of range at %d", pc)
				}
				set(def, localCount+int(output))
			}
		}
		readAddress := func(address AddressPayload) {
			switch address.Kind {
			case "local":
				set(use, lookupLocal(address.Local))
			case "upvalue":
				set(use, lookupUpvalue(address.Upvalue))
			}
			for _, segment := range address.Path {
				if segment.Kind == "index" {
					set(use, lookupLocal(segment.Local))
				}
			}
		}
		branch := func(label, binding string) {
			target, found := labels[label]
			if !found {
				err = fmt.Errorf("reference plan has unknown label %q", label)
				return
			}
			kill := -1
			if binding != "" {
				kill = lookupLocal(binding)
				if kill >= 0 && cellIdentity[kill] {
					// Assigning a new value still needs the existing cell identity.
					set(use, kill)
					kill = -1
				}
			}
			edges[pc] = append(edges[pc], referenceEdge{pc: target, kill: kill})
		}
		fallthroughEdge := true
		switch operation.Op {
		case OpLoadLocal:
			set(use, lookupLocal(operation.Payload.(LocalPayload).Local))
		case OpStoreLocal:
			payload := operation.Payload.(LocalPayload)
			index := lookupLocal(payload.Local)
			set(def, index)
			// Rebinding an addressable source local replaces its cell. A
			// backend storage record for a non-addressable large value keeps
			// its identity, even when source-level assignment starts a new
			// binding; that destination must survive until the write.
			if index >= 0 && cellIdentity[index] && (!payload.Rebind || !plan.Cells[index]) {
				set(use, index)
			}
		case OpLoadUpvalue, OpStoreUpvalue:
			set(use, lookupUpvalue(operation.Payload.(UpvaluePayload).Upvalue))
		case OpAddressOf:
			readAddress(operation.Payload.(AddressPayload))
		case OpMakeClosure:
			for _, capture := range operation.Payload.(ClosurePayload).Captures {
				readAddress(capture)
			}
		case OpTypeDispatch:
			payload := operation.Payload.(TypeDispatchPayload)
			set(use, lookupLocal(payload.Subject))
			branch(payload.Default, payload.DefaultLocal)
			for _, match := range payload.Cases {
				branch(match.Label, match.Binding)
			}
			fallthroughEdge = false
		case OpSelect:
			payload := operation.Payload.(SelectPayload)
			index := lookupLocal(payload.Index)
			set(def, index)
			if index >= 0 && cellIdentity[index] {
				set(use, index)
			}
			for _, selected := range payload.Cases {
				set(use, lookupLocal(selected.Channel))
				if selected.Send != "" {
					set(use, lookupLocal(selected.Send))
				} else {
					// Only the chosen receive writes these locals. Unselected
					// values survive, and addressed destinations retain identity.
					for _, name := range []string{selected.Value, selected.OK} {
						if name != "" {
							index := lookupLocal(name)
							if index >= 0 && cellIdentity[index] {
								set(use, index)
							}
						}
					}
				}
			}
		case OpJump, OpJumpIf:
			branch(operation.Payload.(JumpPayload).Label, "")
			fallthroughEdge = operation.Op != OpJump
		case OpCompareBranch, OpLenCompareBranch:
			branch(operation.Payload.(CompareBranchPayload).Label, "")
		case OpUpdateCompareBranch:
			branch(operation.Payload.(UpdateCompareBranchPayload).Label, "")
		case OpReturn, OpPanic, OpTailCallDirect:
			fallthroughEdge = false
		}
		if fallthroughEdge {
			edges[pc] = append(edges[pc], referenceEdge{pc: pc + 1, kill: -1})
		}
	}
	if err != nil {
		return FrameReferences{}, err
	}
	for changed := true; changed; {
		changed = false
		for pc := count - 1; pc >= 0; pc-- {
			for word := 0; word < words; word++ {
				var outgoing uint64
				for _, edge := range edges[pc] {
					bits := incoming[edge.pc*words+word]
					if edge.kill >= 0 && edge.kill/64 == word {
						bits &^= uint64(1) << (edge.kill % 64)
					}
					outgoing |= bits
				}
				index := pc*words + word
				next := uses[index] | (outgoing &^ defs[index])
				if incoming[index] != next {
					incoming[index], changed = next, true
				}
			}
		}
	}
	for pc, instruction := range fn.Code.Instructions {
		operands := fn.Code.Operands[instruction.Operands]
		for _, slot := range operands.ReleaseBefore {
			if int(slot) >= temporaryCount {
				return FrameReferences{}, fmt.Errorf("reference plan entry release out of range at %d", pc)
			}
			index := localCount + int(slot)
			if incoming[pc*words+index/64]&(uint64(1)<<(index%64)) != 0 {
				return FrameReferences{}, fmt.Errorf("entry release discards live reference slot %d at %d", slot, pc)
			}
		}
		for _, slot := range operands.Release {
			if int(slot) >= temporaryCount {
				return FrameReferences{}, fmt.Errorf("reference plan release out of range at %d", pc)
			}
			index := localCount + int(slot)
			for _, edge := range edges[pc] {
				if incoming[edge.pc*words+index/64]&(uint64(1)<<(index%64)) != 0 {
					return FrameReferences{}, fmt.Errorf("release discards live reference slot %d at %d", slot, pc)
				}
			}
		}
	}
	// Hash buckets avoid a serialized bitmap key allocation per instruction.
	// Equality in each bucket makes interning independent of hash collisions.
	interned := make(map[uint64][]uint32)
	for pc := 0; pc <= count; pc++ {
		bitmap := incoming[pc*words : (pc+1)*words]
		hash := uint64(14695981039346656037)
		for _, word := range bitmap {
			hash = (hash ^ word) * 1099511628211
		}
		found := false
		for _, id := range interned[hash] {
			if slices.Equal(plan.Maps[id], bitmap) {
				plan.AtPC[pc], found = id, true
				break
			}
		}
		if !found {
			id := uint32(len(plan.Maps))
			plan.Maps = append(plan.Maps, slices.Clone(bitmap))
			interned[hash] = append(interned[hash], id)
			plan.AtPC[pc] = id
		}
	}
	return plan, nil
}
