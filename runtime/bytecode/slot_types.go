package bytecode

import (
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

func validateSlotTypes(function Function, operations []Instruction, refs artifactRefs, table *types.TypeTable) error {
	code := function.Code
	relations := types.NewRelations(table)
	locals := make(map[string]types.TypeRef, len(function.Locals))
	upvalues := make(map[string]types.TypeRef, len(function.Upvalues))
	for _, local := range function.Locals {
		locals[local.ID] = local.Type
	}
	for _, upvalue := range function.Upvalues {
		upvalues[upvalue.ID] = upvalue.Type
	}
	addressed := make(map[string]bool)
	for _, operation := range operations {
		var addresses []AddressPayload
		switch payload := operation.Payload.(type) {
		case AddressPayload:
			addresses = []AddressPayload{payload}
		case ClosurePayload:
			addresses = payload.Captures
		}
		for _, address := range addresses {
			if address.Kind == "local" {
				addressed[address.Local] = true
			}
		}
	}
	for pc, operation := range operations {
		operands := code.Operands[code.Instructions[pc].Operands]
		for _, output := range operands.Outputs {
			if output&LocalOutput != 0 {
				local := function.Locals[output&^LocalOutput]
				if addressed[local.ID] || !IsDirectOutputType(table, local.Type) {
					return fmt.Errorf("instruction %d: direct local output requires private fixed-size scalar storage", pc)
				}
			}
		}
		for _, operand := range operands.Inputs {
			if operand.Kind != OperandLocal {
				continue
			}
			local := function.Locals[operand.Index]
			fieldRead := false
			if operation.Op == OpGetPath && len(operands.Outputs) == 1 {
				output := operands.Outputs[0]
				if output&LocalOutput != 0 {
					fieldRead = IsDirectLocalType(table, function.Locals[output&^LocalOutput].Type)
				} else {
					fieldRead = IsDirectLocalType(table, code.Types[output])
				}
			}
			if addressed[local.ID] || !fieldRead && !IsDirectLocalType(table, local.Type) {
				return fmt.Errorf("instruction %d: direct local input requires private scalar, pointer or scalar field read", pc)
			}
		}
		input := func(index int) types.TypeRef {
			operand := operands.Inputs[index]
			if operand.Kind == OperandConstant {
				return refs.constantOrder[operand.Index].Type
			}
			if operand.Kind == OperandLocal {
				return function.Locals[operand.Index].Type
			}
			return code.Types[operand.Index]
		}
		var expectedInputs, expectedOutputs []types.TypeRef
		var integerInputs []int
		switch operation.Op {
		case OpLen, OpCap:
			expectedOutputs = []types.TypeRef{types.Builtin(types.PrimitiveInt)}
		case OpGetPath:
			path := operation.Payload.(FieldPathPayload)
			result, err := fieldPathResult(table, path)
			if err != nil {
				return fmt.Errorf("instruction %d: %w", pc, err)
			}
			expectedInputs, expectedOutputs = []types.TypeRef{path.Type}, []types.TypeRef{result}
		case OpLoadLocal:
			expectedOutputs = []types.TypeRef{locals[operation.Payload.(LocalPayload).Local]}
		case OpStoreLocal:
			expectedInputs = []types.TypeRef{locals[operation.Payload.(LocalPayload).Local]}
		case OpLoadUpvalue:
			expectedOutputs = []types.TypeRef{upvalues[operation.Payload.(UpvaluePayload).Upvalue]}
		case OpStoreUpvalue:
			expectedInputs = []types.TypeRef{upvalues[operation.Payload.(UpvaluePayload).Upvalue]}
		case OpLoadGlobal:
			expectedOutputs = []types.TypeRef{refs.globalTypes[operation.Payload.(GlobalPayload).Global]}
		case OpStoreGlobal:
			expectedInputs = []types.TypeRef{refs.globalTypes[operation.Payload.(GlobalPayload).Global]}
		case OpZero, OpConvert, OpTypeAssert:
			expectedOutputs = []types.TypeRef{operation.Payload.(TypePayload).Type}
		case OpTypeAssertOK:
			expectedOutputs = []types.TypeRef{operation.Payload.(TypePayload).Type, types.Builtin(types.PrimitiveBool)}
		case OpMakeStruct:
			construction := operation.Payload.(MakeStructPayload)
			expectedOutputs = []types.TypeRef{construction.Type}
			fields, _ := types.View(table, construction.Type).StructFields()
			byName := make(map[string]types.TypeRef, len(fields))
			for _, field := range fields {
				byName[field.Name] = field.Type
			}
			for _, name := range construction.Fields {
				fieldType, ok := byName[name]
				if !ok {
					return fmt.Errorf("instruction %d: unknown field %q for %s", pc, name, types.FormatWithTable(table, construction.Type))
				}
				expectedInputs = append(expectedInputs, fieldType)
			}
		case OpMakeSequence:
			construction := operation.Payload.(MakeSequencePayload)
			expectedOutputs = []types.TypeRef{construction.Type}
			view := types.View(table, construction.Type)
			element, ok := view.Elem()
			if !ok {
				_, element, _ = view.Array()
			}
			for range construction.ElementCount {
				expectedInputs = append(expectedInputs, element)
			}
		case OpMakeMap:
			construction := operation.Payload.(MakeMapPayload)
			key, value, _ := types.View(table, construction.Type).Map()
			for range construction.EntryCount {
				expectedInputs = append(expectedInputs, key, value)
			}
			if construction.HasCapacity {
				integerInputs = append(integerInputs, len(expectedInputs))
				expectedInputs = append(expectedInputs, input(len(expectedInputs)))
			}
			expectedOutputs = []types.TypeRef{construction.Type}
		case OpMakeSlice:
			expectedOutputs = []types.TypeRef{operation.Payload.(MakeSlicePayload).Type}
			integerInputs = append(integerInputs, 0)
			if operation.Payload.(MakeSlicePayload).HasCapacity {
				integerInputs = append(integerInputs, 1)
			}
		case OpMakeWaitable:
			expectedOutputs = []types.TypeRef{operation.Payload.(MakeWaitablePayload).Type}
			integerInputs = append(integerInputs, 0)
		case OpReturn:
			expectedInputs = function.Signature.Results
		case OpLoadIndirect, OpStoreIndirect:
			view := types.View(table, input(0))
			element, ok := view.Elem()
			if !ok || view.Shape() != types.Pointer {
				return fmt.Errorf("instruction %d: indirect operand is not a pointer", pc)
			}
			if operation.Op == OpLoadIndirect {
				expectedOutputs = []types.TypeRef{element}
			} else {
				expectedInputs = []types.TypeRef{input(0), element}
			}
		case OpLoadIndex, OpLoadIndexOK, OpStoreIndex:
			view := types.View(table, input(0))
			if view.Shape() == types.Pointer {
				element, _ := view.Elem()
				view = types.View(table, element)
			}
			var element types.TypeRef
			switch view.Shape() {
			case types.Slice:
				element, _ = view.Elem()
			case types.Array:
				_, element, _ = view.Array()
			case types.Map:
				key, value, _ := view.Map()
				element, expectedInputs = value, []types.TypeRef{input(0), key}
			case types.Primitive:
				if primitive, _ := view.Primitive(); primitive == types.PrimitiveString {
					element = types.Builtin(types.PrimitiveUint8)
				}
			}
			if !element.Valid() {
				return fmt.Errorf("instruction %d: operand is not indexable", pc)
			}
			if view.Shape() != types.Map {
				integerInputs = append(integerInputs, 1)
				expectedInputs = []types.TypeRef{input(0), input(1)}
			}
			if operation.Op == OpStoreIndex {
				if view.Shape() == types.Primitive {
					return fmt.Errorf("instruction %d: string index is not writable", pc)
				}
				expectedInputs = append(expectedInputs, element)
			} else {
				expectedOutputs = []types.TypeRef{element}
			}
			if operation.Op == OpLoadIndexOK {
				if view.Shape() != types.Map {
					return fmt.Errorf("instruction %d: comma-ok index requires map", pc)
				}
				expectedOutputs = append(expectedOutputs, types.Builtin(types.PrimitiveBool))
			}
		case OpLoadField, OpStoreField:
			view := types.View(table, input(0))
			if view.Shape() == types.Pointer {
				element, _ := view.Elem()
				view = types.View(table, element)
			}
			fields, ok := view.StructFields()
			var fieldType types.TypeRef
			if ok {
				for _, field := range fields {
					if field.Name == operation.Payload.(FieldPayload).Field {
						fieldType = field.Type
						break
					}
				}
			}
			if !fieldType.Valid() {
				return fmt.Errorf("instruction %d: field is not declared by operand type", pc)
			}
			if operation.Op == OpLoadField {
				expectedOutputs = []types.TypeRef{fieldType}
			} else {
				expectedInputs = []types.TypeRef{input(0), fieldType}
			}
		case OpCallValue:
			signature, ok := types.View(table, input(0)).Function()
			if !ok {
				return fmt.Errorf("instruction %d: callee is not a function", pc)
			}
			expectedInputs = []types.TypeRef{input(0)}
			for _, param := range signature.Params {
				expectedInputs = append(expectedInputs, param.Type)
			}
			expectedOutputs = signature.Results
		case OpWaitableSend, OpWaitableTrySend, OpWaitableRecv, OpWaitableRecvOK,
			OpWaitableTryRecv, OpWaitableCanRecv, OpWaitableCanSend, OpWaitableClose:
			direction, element, ok := types.View(table, input(0)).Waitable()
			if !ok {
				return fmt.Errorf("instruction %d: channel operation requires a channel", pc)
			}
			sending := operation.Op == OpWaitableSend || operation.Op == OpWaitableTrySend || operation.Op == OpWaitableCanSend || operation.Op == OpWaitableClose
			if sending && direction == types.ChannelReceive || !sending && direction == types.ChannelSend {
				return fmt.Errorf("instruction %d: channel direction does not permit operation", pc)
			}
			switch operation.Op {
			case OpWaitableSend, OpWaitableTrySend:
				expectedInputs = []types.TypeRef{input(0), element}
			case OpWaitableRecv, OpWaitableRecvOK, OpWaitableTryRecv:
				expectedOutputs = []types.TypeRef{element}
			}
			if operation.Op == OpWaitableTrySend || operation.Op == OpWaitableRecvOK || operation.Op == OpWaitableTryRecv || operation.Op == OpWaitableCanRecv || operation.Op == OpWaitableCanSend {
				expectedOutputs = append(expectedOutputs, types.Builtin(types.PrimitiveBool))
			}
		case OpStringRuneAt, OpStringNextRuneIndex:
			if primitive, _ := types.View(table, input(0)).Primitive(); primitive != types.PrimitiveString {
				return fmt.Errorf("instruction %d: rune operation requires a string", pc)
			}
			integerInputs = []int{1}
			result := types.PrimitiveInt
			if operation.Op == OpStringRuneAt {
				result = types.PrimitiveInt32
			}
			expectedOutputs = []types.TypeRef{types.Builtin(result)}
		case OpDelete:
			key, _, ok := types.View(table, input(0)).Map()
			if !ok {
				return fmt.Errorf("instruction %d: delete requires a map", pc)
			}
			expectedInputs = []types.TypeRef{input(0), key}
		case OpClear:
			shape := types.View(table, input(0)).Shape()
			if shape != types.Map && shape != types.Slice {
				return fmt.Errorf("instruction %d: clear requires a map or slice", pc)
			}
		case OpJumpIf:
			if primitive, ok := types.View(table, input(0)).Primitive(); !ok || primitive != types.PrimitiveBool {
				return fmt.Errorf("instruction %d: branch condition is not boolean", pc)
			}
		case OpCallDirect, OpTailCallDirect:
			call := operation.Payload.(CallPayload)
			if call.ModulePath != "" && call.ModulePath != refs.modulePath {
				break
			}
			signature := refs.functionSignatures[call.Function]
			for _, param := range signature.Params {
				expectedInputs = append(expectedInputs, param.Type)
			}
			if operation.Op == OpCallDirect {
				expectedOutputs = signature.Results
			}
		case OpUnary, OpBinary:
			arguments := []types.TypeRef{input(0)}
			if operation.Op == OpBinary {
				arguments = append(arguments, input(1))
			}
			result, err := scalarResultType(relations, operation.Payload.(OperatorPayload).Operator, arguments)
			if err != nil {
				return fmt.Errorf("instruction %d: %w", pc, err)
			}
			expectedOutputs = []types.TypeRef{result}
		case OpCompareBranch:
			declared := operation.Payload.(CompareBranchPayload).Type
			if !relations.Identical(input(0), declared).OK {
				return fmt.Errorf("instruction %d: comparison operand type differs from descriptor", pc)
			}
			if _, err := scalarResultType(relations, operation.Payload.(CompareBranchPayload).Operator, []types.TypeRef{input(0), input(1)}); err != nil {
				return fmt.Errorf("instruction %d: %w", pc, err)
			}
		}
		for _, index := range integerInputs {
			primitive, ok := types.View(table, input(index)).Primitive()
			if !ok || primitive < types.PrimitiveInt || primitive > types.PrimitiveUintptr {
				return fmt.Errorf("instruction %d input %d: index or size is not an integer", pc, index)
			}
		}
		if len(expectedInputs) != 0 {
			if len(expectedInputs) != len(operands.Inputs) {
				return fmt.Errorf("instruction %d: input signature mismatch", pc)
			}
			for i, target := range expectedInputs {
				source := input(i)
				if relations.Assignable(source, target).OK || source.Kind == types.Void && relations.NilAssignable(target).OK {
					continue
				}
				return fmt.Errorf("instruction %d input %d: %s is not assignable to %s", pc, i, types.FormatWithTable(table, source), types.FormatWithTable(table, target))
			}
		}
		if len(expectedOutputs) != 0 {
			if len(expectedOutputs) != len(operands.Outputs) {
				return fmt.Errorf("instruction %d: output signature mismatch", pc)
			}
			for i, expected := range expectedOutputs {
				output := operands.Outputs[i]
				var actual types.TypeRef
				if output&LocalOutput != 0 {
					actual = function.Locals[output&^LocalOutput].Type
				} else {
					actual = code.Types[output]
				}
				if !relations.Identical(actual, expected).OK {
					return fmt.Errorf("instruction %d output %d: %s differs from %s", pc, i, types.FormatWithTable(table, actual), types.FormatWithTable(table, expected))
				}
			}
		}
	}
	return nil
}

// scalarResultType validates the operator's runtime contract before a typed
// destination can be trusted by the executor. Shifts keep the left type;
// equality permits interface and nil operands without unboxing their values.
func scalarResultType(relations types.Relations, operator string, operands []types.TypeRef) (types.TypeRef, error) {
	left := operands[0]
	view := relations.View(left)
	primitive, _ := view.Primitive()
	numeric, number := view.NumericInfo()
	integer := number && (numeric.Kind == types.NumericSigned || numeric.Kind == types.NumericUnsigned)
	result, valid := left, false
	if len(operands) == 1 {
		switch operator {
		case "+", "-":
			valid = number
		case "^":
			valid = integer
		case "!":
			valid, result = primitive == types.PrimitiveBool, types.Builtin(types.PrimitiveBool)
		case "real", "imag":
			valid, result = number && numeric.Kind == types.NumericComplex, types.Builtin(types.PrimitiveFloat64)
			if primitive == types.PrimitiveComplex64 {
				result = types.Builtin(types.PrimitiveFloat32)
			}
		}
	} else {
		right := operands[1]
		rightView := relations.View(right)
		rightPrimitive, _ := rightView.Primitive()
		rightNumeric, rightNumber := rightView.NumericInfo()
		matching := relations.Identical(left, right).OK
		switch operator {
		case "==", "!=":
			result = types.Builtin(types.PrimitiveBool)
			valid = left.Kind == types.Void && relations.NilAssignable(right).OK || right.Kind == types.Void && relations.NilAssignable(left).OK ||
				matching && relations.NilAssignable(left).OK ||
				relations.Comparable(left).OK && relations.Comparable(right).OK && (relations.Assignable(left, right).OK || relations.Assignable(right, left).OK)
		case "<", "<=", ">", ">=":
			valid, result = matching && view.Ordered(), types.Builtin(types.PrimitiveBool)
		case "<<", ">>":
			valid = integer && rightNumber && (rightNumeric.Kind == types.NumericSigned || rightNumeric.Kind == types.NumericUnsigned)
		case "&&", "||":
			valid = matching && primitive == types.PrimitiveBool
		case "complex":
			valid, result = matching && number && numeric.Kind == types.NumericFloat, types.Builtin(types.PrimitiveComplex128)
			if primitive == types.PrimitiveFloat32 {
				result = types.Builtin(types.PrimitiveComplex64)
			}
		case "+":
			valid = matching && (number || primitive == types.PrimitiveString && rightPrimitive == types.PrimitiveString)
		case "-", "*", "/":
			valid = matching && number
		case "%", "&", "|", "^", "&^":
			valid = matching && integer
		}
	}
	if !valid {
		return types.TypeRef{}, fmt.Errorf("operator %q has incompatible operand types", operator)
	}
	return result, nil
}

func fieldPathResult(table *types.TypeTable, path FieldPathPayload) (types.TypeRef, error) {
	current := path.Type
	for _, index := range path.Fields {
		view := types.View(table, current)
		if view.Shape() == types.Pointer {
			element, _ := view.Elem()
			view = types.View(table, element)
		}
		fields, ok := view.StructFields()
		if !ok || uint64(index) >= uint64(len(fields)) || fields[index].Name == "_" {
			return types.TypeRef{}, fmt.Errorf("invalid field index %d in path", index)
		}
		current = fields[index].Type
	}
	return current, nil
}
