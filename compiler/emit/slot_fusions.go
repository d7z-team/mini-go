package emit

import (
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// fuseHotPathInstructions applies only adjacent, single-consumer rewrites. The
// source locations and scope boundaries are checked before removing a producer
// so a fused instruction cannot hide a debugger-visible source point.
func fuseHotPathInstructions(code *ir.SlotCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
	if err := fuseIndexConvert(code, function, table, constants, symbols); err != nil {
		return err
	}
	return fuseLenCompareBranch(code, function, table, constants, symbols)
}

func fuseIndexConvert(code *ir.SlotCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
	if len(code.Instructions) < 2 {
		return nil
	}
	uses := slotUseCounts(code)
	boundaries := sourceBoundaries(symbols)
	removed := make([]bool, len(code.Instructions))
	overrides := make(map[int]ir.Instruction)
	for pc := 0; pc+1 < len(code.Instructions); pc++ {
		load, convert := code.Instructions[pc], code.Instructions[pc+1]
		if load.Op != ir.OpLoadIndex || convert.Op != ir.OpConvert || boundaries[pc+1] {
			continue
		}
		loadOperands := code.Operands[load.Operands]
		convertOperands := code.Operands[convert.Operands]
		if len(loadOperands.Inputs) != 2 || len(loadOperands.Outputs) != 1 || len(convertOperands.Inputs) != 1 ||
			loadOperands.Outputs[0]&ir.LocalOutput != 0 || convertOperands.Inputs[0].Kind != ir.OperandSlot ||
			convertOperands.Inputs[0].Index != loadOperands.Outputs[0] || uses[loadOperands.Outputs[0]] != 1 {
			continue
		}
		objectType, ok := slotOperandType(code, function, constants, loadOperands.Inputs[0])
		if !ok || !isByteSequence(table, objectType) {
			continue
		}
		payload, err := code.Descriptors.Payload(convert.Op, convert.Descriptor)
		if err != nil {
			return err
		}
		target, ok := payload.(ir.TypePayload)
		if !ok || !types.IsIntegerType(table, target.Type) {
			continue
		}
		fusedOperands := ir.SlotOperands{
			Inputs:        append([]ir.Operand(nil), loadOperands.Inputs...),
			Outputs:       append([]uint32(nil), convertOperands.Outputs...),
			Release:       mergeSlotReleases(loadOperands.Release, convertOperands.Release, loadOperands.Outputs[0]),
			ReleaseBefore: mergeSlotReleases(loadOperands.ReleaseBefore, convertOperands.ReleaseBefore, loadOperands.Outputs[0]),
		}
		operandsIndex := uint32(len(code.Operands))
		code.Operands = append(code.Operands, fusedOperands)
		overrides[pc+1] = ir.Instruction{Op: ir.OpIndexConvert, Payload: target}
		code.Instructions[pc+1].Operands = operandsIndex
		removed[pc] = true
	}
	if len(overrides) == 0 {
		return nil
	}
	return rewriteSlotCode(code, removed, nil, overrides, symbols)
}

func fuseLenCompareBranch(code *ir.SlotCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
	if len(code.Instructions) < 2 {
		return nil
	}
	uses := slotUseCounts(code)
	boundaries := sourceBoundaries(symbols)
	removed := make([]bool, len(code.Instructions))
	overrides := make(map[int]ir.Instruction)
	for pc := 0; pc+1 < len(code.Instructions); pc++ {
		length, branch := code.Instructions[pc], code.Instructions[pc+1]
		if length.Op != ir.OpLen || branch.Op != ir.OpCompareBranch || boundaries[pc+1] {
			continue
		}
		lengthOperands := code.Operands[length.Operands]
		branchOperands := code.Operands[branch.Operands]
		if len(lengthOperands.Inputs) != 1 || len(lengthOperands.Outputs) != 1 || lengthOperands.Outputs[0]&ir.LocalOutput != 0 ||
			len(branchOperands.Inputs) != 2 || uses[lengthOperands.Outputs[0]] != 1 {
			continue
		}
		lengthSlot := lengthOperands.Outputs[0]
		lengthPosition := -1
		for index, input := range branchOperands.Inputs {
			if input.Kind == ir.OperandSlot && input.Index == lengthSlot {
				lengthPosition = index
				break
			}
		}
		if lengthPosition < 0 {
			continue
		}
		payload, err := code.Descriptors.Payload(branch.Op, branch.Descriptor)
		if err != nil {
			return err
		}
		comparison, ok := payload.(ir.CompareBranchPayload)
		primitive, primitiveOK := types.View(table, comparison.Type).Primitive()
		if !ok || !primitiveOK || primitive != types.PrimitiveInt {
			continue
		}
		other := branchOperands.Inputs[1-lengthPosition]
		if !isIntegerOperand(code, function, constants, table, other) {
			continue
		}
		if lengthPosition == 1 {
			comparison.Operator, ok = inverseComparisonOperator(comparison.Operator)
			if !ok {
				continue
			}
		}
		comparison.Type = types.Builtin(types.PrimitiveInt)
		fusedOperands := ir.SlotOperands{
			Inputs:        []ir.Operand{lengthOperands.Inputs[0], other},
			Release:       mergeSlotReleases(lengthOperands.Release, branchOperands.Release, lengthSlot),
			ReleaseBefore: mergeSlotReleases(lengthOperands.ReleaseBefore, branchOperands.ReleaseBefore, lengthSlot),
		}
		operandsIndex := uint32(len(code.Operands))
		code.Operands = append(code.Operands, fusedOperands)
		overrides[pc+1] = ir.Instruction{Op: ir.OpLenCompareBranch, Payload: comparison}
		code.Instructions[pc+1].Operands = operandsIndex
		removed[pc] = true
	}
	if len(overrides) == 0 {
		return nil
	}
	return rewriteSlotCode(code, removed, nil, overrides, symbols)
}

func sourceBoundaries(symbols *ir.FunctionSymbols) map[int]bool {
	boundaries := make(map[int]bool)
	if symbols == nil {
		return boundaries
	}
	for _, location := range symbols.Locations {
		boundaries[location.PC] = len(location.Points) != 0
	}
	for _, scope := range symbols.Scopes {
		for _, span := range scope.Ranges {
			boundaries[span.Start], boundaries[span.End] = true, true
		}
	}
	return boundaries
}

func slotUseCounts(code *ir.SlotCode) []int {
	uses := make([]int, len(code.Types))
	for _, instruction := range code.Instructions {
		for _, input := range code.Operands[instruction.Operands].Inputs {
			if input.Kind == ir.OperandSlot && int(input.Index) < len(uses) {
				uses[input.Index]++
			}
		}
	}
	return uses
}

func slotOperandType(code *ir.SlotCode, function *hir.Function, constants []ir.Constant, operand ir.Operand) (types.TypeRef, bool) {
	switch operand.Kind {
	case ir.OperandSlot:
		if int(operand.Index) >= len(code.Types) {
			return types.TypeRef{}, false
		}
		return code.Types[operand.Index], true
	case ir.OperandLocal:
		if int(operand.Index) >= len(function.Locals) {
			return types.TypeRef{}, false
		}
		return function.Locals[operand.Index].Type, true
	case ir.OperandConstant:
		if int(operand.Index) >= len(constants) {
			return types.TypeRef{}, false
		}
		return constants[operand.Index].Type, true
	default:
		return types.TypeRef{}, false
	}
}

func isByteSequence(table *types.TypeTable, typ types.TypeRef) bool {
	view := types.View(table, typ)
	if view.Shape() == types.Pointer {
		element, ok := view.Elem()
		if !ok {
			return false
		}
		view = types.View(table, element)
		if view.Shape() != types.Array {
			return false
		}
	}
	if primitive, ok := view.Primitive(); ok {
		return primitive == types.PrimitiveString
	}
	var element types.TypeRef
	switch view.Shape() {
	case types.Slice:
		element, _ = view.Elem()
	case types.Array:
		_, element, _ = view.Array()
	default:
		return false
	}
	primitive, ok := types.View(table, element).Primitive()
	return ok && primitive == types.PrimitiveUint8
}

func isIntegerOperand(code *ir.SlotCode, function *hir.Function, constants []ir.Constant, table *types.TypeTable, operand ir.Operand) bool {
	typ, ok := slotOperandType(code, function, constants, operand)
	return ok && types.IsIntegerType(table, typ)
}

func mergeSlotReleases(first, second []uint32, omitted uint32) []uint32 {
	merged := make([]uint32, 0, len(first)+len(second))
	seen := make(map[uint32]struct{}, len(first)+len(second))
	for _, releases := range [][]uint32{first, second} {
		for _, slot := range releases {
			if slot == omitted {
				continue
			}
			if _, exists := seen[slot]; exists {
				continue
			}
			seen[slot] = struct{}{}
			merged = append(merged, slot)
		}
	}
	return merged
}

func inverseComparisonOperator(operator string) (string, bool) {
	switch operator {
	case "==", "!=":
		return operator, true
	case "<":
		return ">", true
	case "<=":
		return ">=", true
	case ">":
		return "<", true
	case ">=":
		return "<=", true
	default:
		return "", false
	}
}
