package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) evalCompare(operator preparedOperator, left, right vmValue) (vmValue, error) {
	switch operator {
	case operatorEqual, operatorNotEqual:
		equal, err := m.equalValues(left, right)
		if err != nil {
			return vmValue{}, err
		}
		if operator == operatorNotEqual {
			equal = !equal
		}
		return newBoolValue(equal), nil
	}
	if isNumericValue(left) && isNumericValue(right) {
		if isComplexValue(left) || isComplexValue(right) {
			return vmValue{}, fmt.Errorf("operator %s is not ordered for complex values", operator)
		}
		result, err := compareNumericValues(operator, left, right)
		if err != nil {
			return vmValue{}, err
		}
		return newBoolValue(result), nil
	}
	if err := m.requireComparableOperandTypes(left.Type, right.Type); err != nil {
		return vmValue{}, err
	}
	if !m.sameRuntimeType(left.Type, right.Type) {
		return vmValue{}, fmt.Errorf("cannot compare %s and %s", left.Type, right.Type)
	}
	switch m.underlyingRuntimeType(left.Type) {
	case "Bool":
		return vmValue{}, fmt.Errorf("operator %s is not valid for Bool", operator)
	case "String":
		l, lok := left.Data.(string)
		r, rok := right.Data.(string)
		if !lok || !rok {
			return vmValue{}, errors.New("invalid String operands")
		}
		return newBoolValue(compareString(operator, l, r)), nil
	default:
		if isStringValue(left) && isStringValue(right) {
			return newBoolValue(compareString(operator, left.Data.(string), right.Data.(string))), nil
		}
		return vmValue{}, fmt.Errorf("operator %s is not valid for %s", operator, left.Type)
	}
}

func (m *moduleInstance) equalValues(left, right vmValue) (bool, error) {
	return m.equalValuesSeen(left, right, 0)
}

func (m *moduleInstance) equalValuesSeen(left, right vmValue, depth int) (bool, error) {
	if depth > 64 {
		return false, errors.New("comparison nesting limit exceeded")
	}
	leftDynamic := m.isDynamicComparableWrapper(left)
	rightDynamic := m.isDynamicComparableWrapper(right)
	if leftDynamic || rightDynamic {
		leftValue, leftPresent, err := m.unwrapComparableDynamicValue(left)
		if err != nil {
			return false, err
		}
		rightValue, rightPresent, err := m.unwrapComparableDynamicValue(right)
		if err != nil {
			return false, err
		}
		if !leftPresent || !rightPresent {
			return !leftPresent && !rightPresent, nil
		}
		if !m.sameRuntimeType(leftValue.Type, rightValue.Type) {
			return false, nil
		}
		comparable, err := m.isComparableRuntimeType(leftValue.Type, map[string]struct{}{})
		if err != nil {
			return false, err
		}
		if !comparable {
			return false, newGuestPanic(fmt.Errorf("%s is not comparable", leftValue.Type))
		}
		return m.equalConcreteValues(leftValue, rightValue, depth+1)
	}
	return m.equalConcreteValues(left, right, depth+1)
}

func (m *moduleInstance) equalConcreteValues(left, right vmValue, depth int) (bool, error) {
	if left.Type.Ref.Kind == types.Primitive && right.Type.Ref.Kind == types.Primitive {
		if isNumericValue(left) && isNumericValue(right) {
			return equalNumericValues(left, right)
		}
		if !left.Type.Equal(right.Type) {
			return false, fmt.Errorf("cannot compare %s and %s", left.Type, right.Type)
		}
		switch left.Type.Ref.Primitive {
		case types.PrimitiveBool:
			l, lok := left.Data.(bool)
			r, rok := right.Data.(bool)
			if !lok || !rok {
				return false, errors.New("invalid Bool operands")
			}
			return l == r, nil
		case types.PrimitiveString:
			l, lok := left.Data.(string)
			r, rok := right.Data.(string)
			if !lok || !rok {
				return false, errors.New("invalid String operands")
			}
			return l == r, nil
		}
	}
	if err := m.requireComparableOperandTypes(left.Type, right.Type); err != nil {
		return false, err
	}
	if m.predeclaredNumericComparable(left.Type) && m.predeclaredNumericComparable(right.Type) && isNumericValue(left) && isNumericValue(right) {
		return equalNumericValues(left, right)
	}
	if !m.sameRuntimeType(left.Type, right.Type) {
		return false, fmt.Errorf("cannot compare %s and %s", left.Type, right.Type)
	}
	if m.isNilOnlyComparableType(left.Type) {
		leftNil := isNilRuntimeValue(left)
		rightNil := isNilRuntimeValue(right)
		if !leftNil && !rightNil {
			return false, fmt.Errorf("%s is not comparable except to nil", left.Type)
		}
		return leftNil == rightNil, nil
	}
	if m.isReferenceComparableType(left.Type) {
		return referenceComparableIdentity(left.Data) == referenceComparableIdentity(right.Data), nil
	}
	if ok, err := m.isComparableRuntimeType(left.Type, map[string]struct{}{}); err != nil || !ok {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("%s is not comparable", left.Type)
	}
	switch m.underlyingRuntimeType(left.Type) {
	case "Bool":
		l, lok := left.Data.(bool)
		r, rok := right.Data.(bool)
		if !lok || !rok {
			return false, errors.New("invalid Bool operands")
		}
		return l == r, nil
	case "String":
		l, lok := left.Data.(string)
		r, rok := right.Data.(string)
		if !lok || !rok {
			return false, errors.New("invalid String operands")
		}
		return l == r, nil
	case "Float32", "Float64", "Complex64", "Complex128":
		return equalNumericValues(left, right)
	}
	if isIntegerValue(left) && isIntegerValue(right) {
		return equalNumericValues(left, right)
	}
	if _, _, ok := m.arrayType(left.Type); ok {
		leftArray, lok := left.Data.(*vmArray)
		rightArray, rok := right.Data.(*vmArray)
		if !lok || !rok {
			return false, fmt.Errorf("invalid array operands for %s", left.Type)
		}
		leftItems, rightItems := leftArray.values(), rightArray.values()
		if len(leftItems) != len(rightItems) {
			return false, nil
		}
		for i := range leftItems {
			equal, err := m.equalValuesSeen(leftItems[i], rightItems[i], depth+1)
			if err != nil {
				return false, fmt.Errorf("array element %d: %w", i, err)
			}
			if !equal {
				return false, nil
			}
		}
		return true, nil
	}
	if fields, ok := m.comparableStructFields(left.Type); ok {
		if !isStructValue(left.Data) {
			return false, fmt.Errorf("invalid struct operands for %s", left.Type)
		}
		if !isStructValue(right.Data) {
			return false, fmt.Errorf("invalid struct operands for %s", right.Type)
		}
		for _, field := range fields {
			leftField, err := loadFieldValue(m, left, field.Name)
			if err != nil {
				return false, fmt.Errorf("struct field %q: %w", field.Name, err)
			}
			rightField, err := loadFieldValue(m, right, field.Name)
			if err != nil {
				return false, fmt.Errorf("struct field %q: %w", field.Name, err)
			}
			equal, err := m.equalValuesSeen(leftField, rightField, depth+1)
			if err != nil {
				return false, fmt.Errorf("struct field %q: %w", field.Name, err)
			}
			if !equal {
				return false, nil
			}
		}
		return true, nil
	}
	return left.Data == right.Data, nil
}

func (m *moduleInstance) unwrapComparableDynamicValue(value vmValue) (vmValue, bool, error) {
	if value.Type.Ref.Kind == types.Any || m.isInterfaceType(value.Type) {
		return m.unwrapDynamicInterfaceValue(value, "Any")
	}
	return value, true, nil
}

func (m *moduleInstance) isDynamicComparableWrapper(value vmValue) bool {
	return value.Type.Ref.Kind == types.Any || m.isInterfaceType(value.Type)
}

func (m *moduleInstance) requireComparableOperandTypes(left, right any) error {
	leftType := m.resolvedRuntimeType(left)
	rightType := m.resolvedRuntimeType(right)
	if !leftType.Valid() || !rightType.Valid() {
		return nil
	}
	if m.sameRuntimeType(leftType, rightType) {
		return nil
	}
	if m.predeclaredNumericComparable(leftType) && m.predeclaredNumericComparable(rightType) {
		return nil
	}
	if m.isInterfaceType(leftType) || m.isInterfaceType(rightType) || leftType.Ref.Kind == types.Any || rightType.Ref.Kind == types.Any {
		return nil
	}
	return fmt.Errorf("cannot compare %s and %s", leftType, rightType)
}

func (m *moduleInstance) predeclaredNumericComparable(typ any) bool {
	runtimeType := m.resolvedRuntimeType(typ)
	if !runtimeType.Valid() || runtimeType.Ref.Kind == types.Named {
		return false
	}
	_, ok := runtimeType.NumericInfo()
	return ok
}

func (m *moduleInstance) isComparableRuntimeType(typ any, seen map[string]struct{}) (bool, error) {
	runtimeType := m.resolvedRuntimeType(typ)
	if !runtimeType.Valid() {
		return true, nil
	}
	identity := m.runtimeTypeIdentity(runtimeType)
	if _, recursive := seen[identity]; recursive {
		return true, nil
	}
	seen[identity] = struct{}{}
	defer delete(seen, identity)
	if m.isInterfaceType(runtimeType) || runtimeType.Ref.Kind == types.Any {
		return true, nil
	}
	runtimeType = m.resolvedRuntimeType(m.underlyingRuntimeType(runtimeType))
	if runtimeType.Primitive(types.PrimitiveBool) || runtimeType.Primitive(types.PrimitiveString) {
		return true, nil
	}
	if _, ok := runtimeType.NumericInfo(); ok {
		return true, nil
	}
	if runtimeType.ShapeKind() == types.Function || runtimeType.Primitive(types.PrimitiveFunction) {
		return false, nil
	}
	if m.isReferenceComparableType(runtimeType) {
		return true, nil
	}
	if m.isNilOnlyComparableType(runtimeType) {
		return false, nil
	}
	if _, elem, ok := runtimeType.ArrayInfo(); ok {
		return m.isComparableRuntimeType(elem, seen)
	}
	if fields, ok := m.comparableStructFields(runtimeType); ok {
		for _, field := range fields {
			ok, err := m.isComparableRuntimeType(field.Type, seen)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	}
	return true, nil
}

func (m *moduleInstance) comparableStructFields(typ any) ([]structFieldType, bool) {
	runtimeType := m.resolvedRuntimeType(m.underlyingRuntimeType(typ))
	if fields, ok := runtimeStructFieldTypes(runtimeType); ok {
		return fields, true
	}
	if m != nil && m.executable != nil {
		if decl, ok := m.executable.Types[strings.TrimSpace(m.resolvedRuntimeType(typ).String())]; ok {
			if typeFields := m.executable.typeFields(decl); len(typeFields) != 0 {
				fields := make([]structFieldType, 0, len(typeFields))
				for _, field := range typeFields {
					name := strings.TrimSpace(field.Name)
					fieldType := strings.TrimSpace(m.formatType(field.Type))
					if name == "" || name == "_" || fieldType == "" {
						continue
					}
					fields = append(fields, structFieldType{Name: name, Type: fieldType})
				}
				return fields, true
			}
		}
	}
	return nil, false
}

func (m *moduleInstance) isNilOnlyComparableType(typ any) bool {
	runtimeType := m.resolvedRuntimeType(m.underlyingRuntimeType(typ))
	if runtimeType.ShapeKind() == types.Function || runtimeType.Primitive(types.PrimitiveFunction) {
		return true
	}
	switch runtimeType.ShapeKind() {
	case types.Slice, types.Map:
		return true
	}
	return false
}

func (m *moduleInstance) isReferenceComparableType(typ any) bool {
	switch m.resolvedRuntimeType(m.underlyingRuntimeType(typ)).ShapeKind() {
	case types.Pointer, types.Waitable:
		return true
	default:
		return false
	}
}

func isNilRuntimeValue(value vmValue) bool {
	switch data := value.Data.(type) {
	case nil:
		return true
	case *vmSlice:
		return data == nil
	case *vmMap:
		return data == nil
	case *vmPointer:
		return data == nil
	case *waitableResource:
		return data == nil
	default:
		return false
	}
}

func referenceComparableIdentity(data any) any {
	switch value := data.(type) {
	case nil:
		return "nil"
	case *vmPointer:
		if value == nil {
			return "nil"
		}
		if value.Identity != "" {
			return value.Identity
		}
		return value
	case *waitableResource:
		if value == nil {
			return "nil"
		}
		return value
	}
	return data
}

func referenceComparableMapKeyIdentity(data any) (string, error) {
	switch value := data.(type) {
	case *mutexResource:
		if value == nil {
			return "nil", nil
		}
		return fmt.Sprintf("mutex:%p", value), nil
	case nil:
		return "nil", nil
	case *vmPointer:
		if value == nil {
			return "nil", nil
		}
		if value.Identity != "" {
			return "pointer:" + value.Identity, nil
		}
		return fmt.Sprintf("pointer:%p", value), nil
	case *waitableResource:
		if value == nil {
			return "nil", nil
		}
		return fmt.Sprintf("waitable:%p", value), nil
	default:
		return "", fmt.Errorf("unsupported reference map key data %T", data)
	}
}
