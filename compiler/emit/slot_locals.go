package emit

import (
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// forwardLocalOperands removes temporary copies whose local remains unchanged
// until every use in the same control-flow region. Captured/addressed cells
// always retain their explicit reads, including across calls that can mutate
// them. The backward write index preserves parallel assignment snapshots.
func forwardLocalOperands(code *ir.SlotCode, function *hir.Function, table *types.TypeTable, symbols *ir.FunctionSymbols) error {
	locals := make(map[string]int, len(function.Locals))
	private := make([]bool, len(function.Locals))
	nextWrite := make([]int, len(function.Locals))
	for i, local := range function.Locals {
		locals[local.ID] = i
		private[i] = true
		nextWrite[i] = len(code.Instructions)
	}
	for _, address := range code.Descriptors.Address {
		if address.Kind == "local" {
			private[locals[address.Local]] = false
		}
	}
	for _, closure := range code.Descriptors.Closure {
		for _, address := range closure.Captures {
			if address.Kind == "local" {
				private[locals[address.Local]] = false
			}
		}
	}
	if err := promoteTemporaries(code, function, table, locals, private, symbols); err != nil {
		return err
	}
	for i := range nextWrite {
		nextWrite[i] = len(code.Instructions)
	}
	lastUse := make([]int, len(code.Types))
	fieldReads := make([]bool, len(code.Types))
	for i := range lastUse {
		lastUse[i] = -1
		fieldReads[i] = true
	}
	for pc, instruction := range code.Instructions {
		operands := code.Operands[instruction.Operands]
		fieldRead := instruction.Op == ir.OpGetPath && len(operands.Outputs) == 1 && ir.IsDirectLocalType(table, code.Types[operands.Outputs[0]])
		for _, input := range operands.Inputs {
			if input.Kind == ir.OperandSlot {
				fieldReads[input.Index] = fieldReads[input.Index] && fieldRead
				lastUse[input.Index] = pc
			}
		}
	}
	forwarded := make(map[uint32]ir.Operand)
	removed := make([]bool, len(code.Instructions))
	boundary := len(code.Instructions)
	for pc := len(code.Instructions) - 1; pc >= 0; pc-- {
		instruction := code.Instructions[pc]
		switch instruction.Op {
		case ir.OpStoreLocal:
			local := code.Descriptors.Local[instruction.Descriptor]
			nextWrite[locals[local.Local]] = pc
		case ir.OpLoadLocal:
			local := locals[code.Descriptors.Local[instruction.Descriptor].Local]
			output := code.Operands[instruction.Operands].Outputs[0]
			if !private[local] || !fieldReads[output] && !ir.IsDirectLocalType(table, function.Locals[local].Type) {
				continue
			}
			if lastUse[output] <= boundary && lastUse[output] <= nextWrite[local] {
				forwarded[output], removed[pc] = ir.Operand{Kind: ir.OperandLocal, Index: uint32(local)}, true
			}
		case ir.OpLabel, ir.OpJump, ir.OpJumpIf, ir.OpCompareBranch, ir.OpReturn, ir.OpPanic,
			ir.OpTypeDispatch, ir.OpSelect, ir.OpMapIterInit, ir.OpMapIterNext, ir.OpMapIterClose,
			ir.OpCallDirect, ir.OpCallValue, ir.OpCallInterface, ir.OpTailCallDirect, ir.OpCallFFI,
			ir.OpDeferPush, ir.OpSpawn, ir.OpInitModule:
			boundary = pc
		}
	}
	if len(forwarded) != 0 {
		if err := rewriteSlotLocals(code, removed, forwarded, symbols); err != nil {
			return err
		}
	}
	pruneUnusedTemporaryLocals(code, function, symbols)
	return nil
}

// Implicit operands keep cells alive just like ordinary loads and stores.
// Compact declarations only after rewriting operands, and retain source locals
// for inspection even when their current value is not used by guest code.
func pruneUnusedTemporaryLocals(code *ir.SlotCode, function *hir.Function, symbols *ir.FunctionSymbols) {
	used := make(map[string]bool, len(function.Locals))
	for index, local := range function.Locals {
		if index < len(function.Signature.Params) || !local.Generated {
			used[local.ID] = true
		}
	}
	for _, local := range function.ResultLocals {
		used[local] = true
	}
	for _, local := range code.Descriptors.Local {
		used[local.Local] = true
	}
	markAddress := func(address ir.AddressPayload) {
		if address.Kind == "local" {
			used[address.Local] = true
		}
		for _, step := range address.Path {
			if step.Local != "" {
				used[step.Local] = true
			}
		}
	}
	for _, address := range code.Descriptors.Address {
		markAddress(address)
	}
	for _, closure := range code.Descriptors.Closure {
		for _, capture := range closure.Captures {
			markAddress(capture)
		}
	}
	for _, dispatch := range code.Descriptors.TypeDispatch {
		used[dispatch.Subject], used[dispatch.DefaultLocal] = true, true
		for _, choice := range dispatch.Cases {
			used[choice.Binding] = true
		}
	}
	for _, selection := range code.Descriptors.Select {
		used[selection.Index] = true
		for _, choice := range selection.Cases {
			used[choice.Channel], used[choice.Send], used[choice.Value], used[choice.OK] = true, true, true, true
		}
	}
	for _, operands := range code.Operands {
		for _, input := range operands.Inputs {
			if input.Kind == ir.OperandLocal {
				used[function.Locals[input.Index].ID] = true
			}
		}
	}
	count := 0
	for _, local := range function.Locals {
		if used[local.ID] {
			count++
		}
	}
	if count == len(function.Locals) {
		return
	}
	remap := make([]uint32, len(function.Locals))
	locals := make([]hir.Local, 0, count)
	for index, local := range function.Locals {
		if used[local.ID] {
			remap[index] = uint32(len(locals))
			locals = append(locals, local)
		}
	}
	function.Locals = locals
	for i := range code.Operands {
		for j := range code.Operands[i].Inputs {
			input := &code.Operands[i].Inputs[j]
			if input.Kind == ir.OperandLocal {
				input.Index = remap[input.Index]
			}
		}
	}
	if symbols != nil {
		locals := symbols.Locals[:0]
		for _, local := range symbols.Locals {
			if used[local.ID] {
				locals = append(locals, local)
			}
		}
		clear(symbols.Locals[len(locals):])
		symbols.Locals = locals
	}
}

// Scalar/pointer temporaries and uniquely consumed fresh aggregates can retain their
// producer slot instead of allocating a local cell. Source locals, parameters,
// return cells and implicit operands retain their storage and debugger identity.
func promoteTemporaries(code *ir.SlotCode, function *hir.Function, table *types.TypeTable, locals map[string]int, private []bool, symbols *ir.FunctionSymbols) error {
	candidates := make([]bool, len(function.Locals))
	writes := make([]int, len(candidates))
	reads := make([][]int, len(candidates))
	for i, local := range function.Locals {
		candidates[i] = private[i] && local.Generated && i >= len(function.Signature.Params)
		writes[i] = -1
	}
	for _, name := range function.ResultLocals {
		candidates[locals[name]] = false
	}
	for _, address := range code.Descriptors.Address {
		for _, step := range address.Path {
			if step.Local != "" {
				candidates[locals[step.Local]] = false
			}
		}
	}
	for _, closure := range code.Descriptors.Closure {
		for _, address := range closure.Captures {
			for _, step := range address.Path {
				if step.Local != "" {
					candidates[locals[step.Local]] = false
				}
			}
		}
	}
	for _, selection := range code.Descriptors.Select {
		candidates[locals[selection.Index]] = false
		for _, choice := range selection.Cases {
			for _, name := range []string{choice.Channel, choice.Send, choice.Value, choice.OK} {
				if name != "" {
					candidates[locals[name]] = false
				}
			}
		}
	}
	for _, dispatch := range code.Descriptors.TypeDispatch {
		candidates[locals[dispatch.Subject]] = false
		if dispatch.DefaultLocal != "" {
			candidates[locals[dispatch.DefaultLocal]] = false
		}
		for _, choice := range dispatch.Cases {
			if choice.Binding != "" {
				candidates[locals[choice.Binding]] = false
			}
		}
	}
	blocks := make([]int, len(code.Instructions))
	producers := make([]ir.Opcode, len(code.Types))
	definitions := make([]int, len(code.Types))
	uses := make([]int, len(code.Types))
	block := 0
	for pc, instruction := range code.Instructions {
		operands := code.Operands[instruction.Operands]
		for _, output := range operands.Outputs {
			definitions[output]++
			producers[output] = instruction.Op
		}
		for _, input := range operands.Inputs {
			if input.Kind == ir.OperandSlot {
				uses[input.Index]++
			}
		}
		if instruction.Op == ir.OpLabel {
			block++
		}
		blocks[pc] = block
		switch instruction.Op {
		case ir.OpStoreLocal:
			local := locals[code.Descriptors.Local[instruction.Descriptor].Local]
			if writes[local] >= 0 {
				candidates[local] = false
			}
			writes[local] = pc
		case ir.OpLoadLocal:
			local := locals[code.Descriptors.Local[instruction.Descriptor].Local]
			reads[local] = append(reads[local], pc)
		case ir.OpJump, ir.OpJumpIf, ir.OpCompareBranch, ir.OpReturn, ir.OpPanic, ir.OpTypeDispatch:
			block++
		}
	}
	removed := make([]bool, len(code.Instructions))
	replacements := make(map[uint32]ir.Operand)
	relations := types.NewRelations(table)
	for local, candidate := range candidates {
		pc := writes[local]
		if !candidate || pc < 0 || len(reads[local]) == 0 {
			continue
		}
		value := code.Operands[code.Instructions[pc].Operands].Inputs[0]
		if value.Kind != ir.OperandSlot || !relations.Identical(code.Types[value.Index], function.Locals[local].Type).OK {
			continue
		}
		if !ir.IsDirectLocalType(table, function.Locals[local].Type) {
			shape := relations.View(function.Locals[local].Type).Shape()
			fresh := shape == types.Struct && producers[value.Index] == ir.OpMakeStruct || shape == types.Array && producers[value.Index] == ir.OpMakeSequence
			if !fresh || definitions[value.Index] != 1 || uses[value.Index] != 1 {
				continue
			}
		}
		for _, read := range reads[local] {
			if read <= pc || blocks[read] != blocks[pc] {
				candidate = false
				break
			}
		}
		if !candidate {
			continue
		}
		removed[pc] = true
		for _, read := range reads[local] {
			output := code.Operands[code.Instructions[read].Operands].Outputs[0]
			replacements[output], removed[read] = value, true
		}
	}
	if len(replacements) == 0 {
		return nil
	}
	return rewriteSlotLocals(code, removed, replacements, symbols)
}

// directLocalResults commits a pure producer to its sole adjacent local store.
// Labels cannot intervene, and no source boundary may enter the eliminated store.
func directLocalResults(code *ir.SlotCode, function *hir.Function, table *types.TypeTable, symbols *ir.FunctionSymbols) error {
	locals := make(map[string]int, len(function.Locals))
	private := make([]bool, len(function.Locals))
	for index, local := range function.Locals {
		locals[local.ID], private[index] = index, ir.IsDirectOutputType(table, local.Type)
	}
	for _, address := range code.Descriptors.Address {
		if address.Kind == "local" {
			private[locals[address.Local]] = false
		}
	}
	for _, closure := range code.Descriptors.Closure {
		for _, capture := range closure.Captures {
			if capture.Kind == "local" {
				private[locals[capture.Local]] = false
			}
		}
	}
	uses := make([]int, len(code.Types))
	for _, instruction := range code.Instructions {
		for _, input := range code.Operands[instruction.Operands].Inputs {
			if input.Kind == ir.OperandSlot {
				uses[input.Index]++
			}
		}
	}
	boundaries := make(map[int]bool)
	if symbols != nil {
		for _, location := range symbols.Locations {
			boundaries[location.PC] = len(location.Points) != 0
		}
		for _, scope := range symbols.Scopes {
			for _, span := range scope.Ranges {
				boundaries[span.Start], boundaries[span.End] = true, true
			}
		}
	}
	removed := make([]bool, len(code.Instructions))
	changed := false
	for pc := 1; pc < len(code.Instructions); pc++ {
		store, producer := code.Instructions[pc], code.Instructions[pc-1]
		if store.Op != ir.OpStoreLocal || !ir.HasDirectOutput(producer.Op) || boundaries[pc] {
			continue
		}
		local := locals[code.Descriptors.Local[store.Descriptor].Local]
		input, output := code.Operands[store.Operands].Inputs, code.Operands[producer.Operands].Outputs
		if !private[local] || len(input) != 1 || input[0].Kind != ir.OperandSlot || len(output) != 1 ||
			input[0].Index != output[0] || uses[output[0]] != 1 || code.Types[output[0]] != function.Locals[local].Type {
			continue
		}
		code.Operands[producer.Operands].Outputs[0] = ir.LocalOutput | uint32(local)
		removed[pc], changed = true, true
	}
	if changed {
		return rewriteSlotLocals(code, removed, nil, symbols)
	}
	return nil
}

func rewriteSlotLocals(code *ir.SlotCode, removed []bool, replacements map[uint32]ir.Operand, symbols *ir.FunctionSymbols) error {
	pcs := make([]int, len(code.Instructions)+1)
	instructions := code.Instructions[:0]
	operands := make([]ir.SlotOperands, 0, len(code.Instructions))
	var descriptors ir.DescriptorTables
	for pc, instruction := range code.Instructions {
		pcs[pc] = len(instructions)
		if removed[pc] {
			continue
		}
		arguments := code.Operands[instruction.Operands]
		for i, input := range arguments.Inputs {
			for input.Kind == ir.OperandSlot {
				replacement, ok := replacements[input.Index]
				if !ok {
					break
				}
				input = replacement
			}
			arguments.Inputs[i] = input
		}
		payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		if err != nil {
			return err
		}
		instruction.Descriptor, err = descriptors.Append(payload)
		if err != nil {
			return err
		}
		instruction.Operands = uint32(len(operands))
		operands = append(operands, arguments)
		instructions = append(instructions, instruction)
	}
	pcs[len(code.Instructions)] = len(instructions)
	code.Instructions, code.Operands, code.Descriptors = instructions, operands, descriptors
	if symbols != nil {
		for i := range symbols.Locations {
			symbols.Locations[i].PC = pcs[symbols.Locations[i].PC]
		}
		for i := range symbols.Scopes {
			for j := range symbols.Scopes[i].Ranges {
				scope := &symbols.Scopes[i].Ranges[j]
				scope.Start, scope.End = pcs[scope.Start], pcs[scope.End]
			}
		}
	}
	return nil
}
