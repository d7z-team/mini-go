package emit

import (
	"strconv"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// fuseHotPathInstructions applies only adjacent, single-consumer rewrites. The
// source locations and scope boundaries are checked before removing a producer
// so a fused instruction cannot hide a debugger-visible source point.
func fuseHotPathInstructions(code *ir.MicroCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
	if err := fuseLoopLatch(code, function, table, constants, symbols); err != nil {
		return err
	}
	if err := fuseUpdateCompareBranch(code, function, table, constants, symbols); err != nil {
		return err
	}
	if err := fuseIndexConvert(code, function, table, constants, symbols); err != nil {
		return err
	}
	return fuseLenCompareBranch(code, function, table, constants, symbols)
}

// fuseLoopLatch folds the canonical loop-tail sequence
//
//	local = local +/- step; jump condition
//
// into an update_compare_branch that returns directly to the loop body while
// the condition holds. The original header comparison remains as the initial
// guard. Later iterations avoid both the back-edge jump and the repeated
// header dispatch without changing logical fuel accounting.
func fuseLoopLatch(code *ir.MicroCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
	if len(code.Instructions) < 5 {
		return nil
	}
	labels := make(map[string]int)
	for pc, instruction := range code.Instructions {
		if instruction.Op != ir.OpLabel {
			continue
		}
		payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		if err != nil {
			return err
		}
		labels[payload.(ir.LabelPayload).Label] = pc
	}
	removed := make([]bool, len(code.Instructions))
	overrides := make(map[int]ir.Instruction)
	insertions := make(map[int][]ir.Instruction)
	conditionPoints := make(map[int][]ir.Location)
	if symbols != nil {
		for _, location := range symbols.Locations {
			conditionPoints[location.PC] = location.Points
		}
	}
	for jumpPC := 2; jumpPC < len(code.Instructions); jumpPC++ {
		jump := code.Instructions[jumpPC]
		if jump.Op != ir.OpJump || removed[jumpPC] {
			continue
		}
		jumpPayload, err := code.Descriptors.Payload(jump.Op, jump.Descriptor)
		if err != nil {
			return err
		}
		headerPC, ok := labels[jumpPayload.(ir.JumpPayload).Label]
		if !ok || headerPC+1 >= jumpPC {
			continue
		}
		conditionPC := headerPC + 1
		condition := code.Instructions[conditionPC]
		if condition.Op != ir.OpCompareBranch {
			continue
		}
		comparisonPayload, err := code.Descriptors.Payload(condition.Op, condition.Descriptor)
		if err != nil {
			return err
		}
		comparison := comparisonPayload.(ir.CompareBranchPayload)
		exitPC, ok := labels[comparison.Label]
		if !ok || exitPC <= jumpPC {
			continue
		}
		// The fused instruction has one branch target and falls through when
		// the loop finishes. It can replace the back edge only when that
		// fallthrough is the original condition's exit. A loop nested in a
		// switch commonly jumps over the remaining cases to a shared exit;
		// fusing that latch would otherwise execute the next case.
		exitFollowsLatch := true
		for pc := jumpPC + 1; pc < exitPC; pc++ {
			if code.Instructions[pc].Op != ir.OpLabel {
				exitFollowsLatch = false
				break
			}
		}
		if !exitFollowsLatch {
			continue
		}
		updatePC := jumpPC - 1
		update := code.Instructions[updatePC]
		if update.Op != ir.OpBinary {
			continue
		}
		updatePayload, err := code.Descriptors.Payload(update.Op, update.Descriptor)
		if err != nil {
			return err
		}
		updateOperator := updatePayload.(ir.OperatorPayload).Operator
		if updateOperator != "+" && updateOperator != "-" {
			continue
		}
		updateFlow := code.Operands[update.Operands]
		conditionFlow := code.Operands[condition.Operands]
		if len(updateFlow.Inputs) != 2 || len(updateFlow.Outputs) != 1 ||
			updateFlow.Outputs[0]&ir.LocalOutput == 0 || len(conditionFlow.Inputs) != 2 {
			continue
		}
		local := updateFlow.Outputs[0] &^ ir.LocalOutput
		if int(local) >= len(function.Locals) || updateFlow.Inputs[0].Kind != ir.OperandLocal ||
			updateFlow.Inputs[0].Index != local || updateFlow.Inputs[1].Kind == ir.OperandSlot {
			continue
		}
		conditionLocal := -1
		for index, input := range conditionFlow.Inputs {
			if input.Kind == ir.OperandLocal && input.Index == local {
				conditionLocal = index
				break
			}
		}
		if conditionLocal < 0 || conditionFlow.Inputs[1-conditionLocal].Kind == ir.OperandSlot {
			continue
		}
		if conditionLocal == 1 {
			comparison.Operator, ok = inverseComparisonOperator(comparison.Operator)
			if !ok {
				continue
			}
		}
		typ := function.Locals[local].Type
		if (!types.IsIntegerType(table, typ) && !types.IsFloatType(table, typ)) ||
			!sameOperandType(code, function, constants, table, updateFlow.Inputs[1], typ) ||
			!sameOperandType(code, function, constants, table, conditionFlow.Inputs[1-conditionLocal], typ) {
			continue
		}

		bodyPC := conditionPC + 1
		var bodyLabel string
		if bodyPC < len(code.Instructions) && code.Instructions[bodyPC].Op == ir.OpLabel {
			payload, err := code.Descriptors.Payload(ir.OpLabel, code.Instructions[bodyPC].Descriptor)
			if err != nil {
				return err
			}
			bodyLabel = payload.(ir.LabelPayload).Label
		} else {
			baseLabel := ".fused.loop.body." + strconv.Itoa(headerPC)
			for suffix := 0; ; suffix++ {
				bodyLabel = baseLabel
				if suffix != 0 {
					bodyLabel += "." + strconv.Itoa(suffix)
				}
				if _, exists := labels[bodyLabel]; !exists {
					break
				}
			}
			labels[bodyLabel] = bodyPC
			insertions[bodyPC] = append(insertions[bodyPC], ir.Instruction{
				Op: ir.OpLabel, Payload: ir.LabelPayload{Label: bodyLabel},
			})
		}
		flow := ir.RegisterFlow{
			Inputs:        []ir.Operand{updateFlow.Inputs[0], updateFlow.Inputs[1], conditionFlow.Inputs[1-conditionLocal]},
			Release:       mergeSlotReleases(updateFlow.Release, conditionFlow.Release, ^uint32(0)),
			ReleaseBefore: mergeSlotReleases(updateFlow.ReleaseBefore, conditionFlow.ReleaseBefore, ^uint32(0)),
		}
		code.Operands = append(code.Operands, flow)
		code.Instructions[updatePC].Operands = uint32(len(code.Operands) - 1)
		code.Instructions[updatePC].Cost = update.LogicalCost() + condition.LogicalCost() + jump.LogicalCost()
		overrides[updatePC] = ir.Instruction{Op: ir.OpUpdateCompareBranch, Payload: ir.UpdateCompareBranchPayload{
			Update: updateOperator, Comparison: comparison.Operator, Type: typ, Label: bodyLabel, When: !comparison.When,
		}}
		removed[jumpPC] = true
		if symbols != nil && len(conditionPoints[conditionPC]) != 0 {
			symbols.Locations = append(symbols.Locations, ir.InstructionSymbol{
				PC: updatePC, Points: append([]ir.Location(nil), conditionPoints[conditionPC]...),
			})
		}
	}
	if len(overrides) == 0 {
		return nil
	}
	return rewriteMicroCodeWithInsertions(code, removed, nil, overrides, insertions, symbols)
}

// fuseUpdateCompareBranch recognizes the canonical counter loop after direct
// result placement has removed the intervening store_local. The fused opcode
// keeps the two source operations' logical cost while eliminating one dispatch
// and the temporary publication boundary.
func fuseUpdateCompareBranch(code *ir.MicroCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
	if len(code.Instructions) < 2 {
		return nil
	}
	boundaries := sourceBoundaries(symbols)
	removed := make([]bool, len(code.Instructions))
	overrides := make(map[int]ir.Instruction)
	for pc := 0; pc+1 < len(code.Instructions); pc++ {
		update, branch := code.Instructions[pc], code.Instructions[pc+1]
		if update.Op != ir.OpBinary || branch.Op != ir.OpCompareBranch || boundaries[pc+1] {
			continue
		}
		updateFlow := code.Operands[update.Operands]
		branchFlow := code.Operands[branch.Operands]
		if len(updateFlow.Inputs) != 2 || len(updateFlow.Outputs) != 1 ||
			updateFlow.Outputs[0]&ir.LocalOutput == 0 || len(branchFlow.Inputs) != 2 {
			continue
		}
		local := updateFlow.Outputs[0] &^ ir.LocalOutput
		if int(local) >= len(function.Locals) || updateFlow.Inputs[0].Kind != ir.OperandLocal || updateFlow.Inputs[0].Index != local {
			continue
		}
		operatorPayload, err := code.Descriptors.Payload(update.Op, update.Descriptor)
		if err != nil {
			return err
		}
		updateOperator := operatorPayload.(ir.OperatorPayload).Operator
		if updateOperator != "+" && updateOperator != "-" {
			continue
		}
		branchLocal := -1
		for index, input := range branchFlow.Inputs {
			if input.Kind == ir.OperandLocal && input.Index == local {
				branchLocal = index
				break
			}
		}
		if branchLocal < 0 {
			continue
		}
		comparisonPayload, err := code.Descriptors.Payload(branch.Op, branch.Descriptor)
		if err != nil {
			return err
		}
		comparison := comparisonPayload.(ir.CompareBranchPayload)
		if branchLocal == 1 {
			var ok bool
			comparison.Operator, ok = inverseComparisonOperator(comparison.Operator)
			if !ok {
				continue
			}
		}
		typ := function.Locals[local].Type
		if !types.IsIntegerType(table, typ) && !types.IsFloatType(table, typ) ||
			!sameOperandType(code, function, constants, table, updateFlow.Inputs[1], typ) ||
			!sameOperandType(code, function, constants, table, branchFlow.Inputs[1-branchLocal], typ) {
			continue
		}
		flow := ir.RegisterFlow{
			Inputs:        []ir.Operand{updateFlow.Inputs[0], updateFlow.Inputs[1], branchFlow.Inputs[1-branchLocal]},
			Release:       mergeSlotReleases(updateFlow.Release, branchFlow.Release, ^uint32(0)),
			ReleaseBefore: mergeSlotReleases(updateFlow.ReleaseBefore, branchFlow.ReleaseBefore, ^uint32(0)),
		}
		code.Operands = append(code.Operands, flow)
		code.Instructions[pc+1].Operands = uint32(len(code.Operands) - 1)
		code.Instructions[pc+1].Cost = update.LogicalCost() + branch.LogicalCost()
		overrides[pc+1] = ir.Instruction{Op: ir.OpUpdateCompareBranch, Payload: ir.UpdateCompareBranchPayload{
			Update: updateOperator, Comparison: comparison.Operator, Type: typ, Label: comparison.Label, When: comparison.When,
		}}
		removed[pc] = true
	}
	if len(overrides) == 0 {
		return nil
	}
	return rewriteMicroCode(code, removed, nil, overrides, symbols)
}

func sameOperandType(code *ir.MicroCode, function *hir.Function, constants []ir.Constant, table *types.TypeTable, operand ir.Operand, expected types.TypeRef) bool {
	typ, ok := slotOperandType(code, function, constants, operand)
	return ok && types.NewRelations(table).Identical(typ, expected).OK
}

func fuseIndexConvert(code *ir.MicroCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
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
		fusedOperands := ir.RegisterFlow{
			Inputs:        append([]ir.Operand(nil), loadOperands.Inputs...),
			Outputs:       append([]uint32(nil), convertOperands.Outputs...),
			Release:       mergeSlotReleases(loadOperands.Release, convertOperands.Release, loadOperands.Outputs[0]),
			ReleaseBefore: mergeSlotReleases(loadOperands.ReleaseBefore, convertOperands.ReleaseBefore, loadOperands.Outputs[0]),
		}
		operandsIndex := uint32(len(code.Operands))
		code.Operands = append(code.Operands, fusedOperands)
		overrides[pc+1] = ir.Instruction{Op: ir.OpIndexConvert, Payload: target}
		code.Instructions[pc+1].Operands = operandsIndex
		code.Instructions[pc+1].Cost = load.LogicalCost() + convert.LogicalCost()
		removed[pc] = true
	}
	if len(overrides) == 0 {
		return nil
	}
	return rewriteMicroCode(code, removed, nil, overrides, symbols)
}

func fuseLenCompareBranch(code *ir.MicroCode, function *hir.Function, table *types.TypeTable, constants []ir.Constant, symbols *ir.FunctionSymbols) error {
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
		otherType, ok := slotOperandType(code, function, constants, other)
		if !ok || !types.IsIntegerType(table, otherType) {
			continue
		}
		if lengthPosition == 1 {
			comparison.Operator, ok = inverseComparisonOperator(comparison.Operator)
			if !ok {
				continue
			}
		}
		comparison.Type = types.Builtin(types.PrimitiveInt)
		fusedOperands := ir.RegisterFlow{
			Inputs:        []ir.Operand{lengthOperands.Inputs[0], other},
			Release:       mergeSlotReleases(lengthOperands.Release, branchOperands.Release, lengthSlot),
			ReleaseBefore: mergeSlotReleases(lengthOperands.ReleaseBefore, branchOperands.ReleaseBefore, lengthSlot),
		}
		operandsIndex := uint32(len(code.Operands))
		code.Operands = append(code.Operands, fusedOperands)
		overrides[pc+1] = ir.Instruction{Op: ir.OpLenCompareBranch, Payload: comparison}
		code.Instructions[pc+1].Operands = operandsIndex
		code.Instructions[pc+1].Cost = length.LogicalCost() + branch.LogicalCost()
		removed[pc] = true
	}
	if len(overrides) == 0 {
		return nil
	}
	return rewriteMicroCode(code, removed, nil, overrides, symbols)
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

func slotUseCounts(code *ir.MicroCode) []int {
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

func slotOperandType(code *ir.MicroCode, function *hir.Function, constants []ir.Constant, operand ir.Operand) (types.TypeRef, bool) {
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
