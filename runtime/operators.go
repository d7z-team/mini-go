package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) evalUnary(operator preparedOperator, value vmValue, numeric types.PrimitiveKind) (vmValue, error) {
	resultType := value.Type
	value = m.numericOperandValue(value, numeric)
	switch operator {
	case operatorNot:
		out, ok := value.Data.(bool)
		if !ok {
			return vmValue{}, fmt.Errorf("operator %s expects Bool, got %s", operator, value.Type)
		}
		return newBoolValue(!out), nil
	case operatorReal, operatorImag:
		if !isComplexValue(value) {
			return vmValue{}, fmt.Errorf("operator %s expects complex value, got %s", operator, value.Type)
		}
		out, err := numericAsComplex128(value)
		if err != nil {
			return vmValue{}, err
		}
		part := real(out)
		if operator == operatorImag {
			part = imag(out)
		}
		resultType := "Float64"
		if value.Type.Primitive(types.PrimitiveComplex64) {
			resultType = "Float32"
		}
		return normalizeFloatValue(coerceRuntimeType(resultType), part), nil
	case operatorSub:
		if isComplexValue(value) {
			out, err := numericAsComplex128(value)
			if err != nil {
				return vmValue{}, err
			}
			return normalizeUnaryNumericResult(resultType, normalizeComplexValue(value.Type, -out)), nil
		}
		if isFloatValue(value) {
			out, err := numericAsFloat64(value)
			if err != nil {
				return vmValue{}, err
			}
			return normalizeUnaryNumericResult(resultType, normalizeFloatValue(value.Type, -out)), nil
		}
		if isUnsignedIntegerValue(value) {
			out, err := numericAsUint64(value)
			if err != nil {
				return vmValue{}, err
			}
			return normalizeUnaryNumericResult(resultType, normalizeUnsignedValue(value.Type, -out)), nil
		}
		if isSignedIntegerValue(value) {
			out, err := numericAsInt64(value)
			if err != nil {
				return vmValue{}, err
			}
			return normalizeUnaryNumericResult(resultType, normalizeSignedValue(value.Type, -out)), nil
		}
		return vmValue{}, fmt.Errorf("operator %s expects numeric value, got %s", operator, value.Type)
	case operatorBitXor:
		if isUnsignedIntegerValue(value) {
			out, err := numericAsUint64(value)
			if err != nil {
				return vmValue{}, err
			}
			return normalizeUnaryNumericResult(resultType, normalizeUnsignedValue(value.Type, ^out)), nil
		}
		if isSignedIntegerValue(value) {
			out, err := numericAsInt64(value)
			if err != nil {
				return vmValue{}, err
			}
			return normalizeUnaryNumericResult(resultType, normalizeSignedValue(value.Type, ^out)), nil
		}
		return vmValue{}, fmt.Errorf("operator %s expects integer value, got %s", operator, value.Type)
	default:
		return vmValue{}, fmt.Errorf("unsupported unary operator %q", operator)
	}
}

func (m *moduleInstance) evalBinary(operator preparedOperator, left, right vmValue, numeric [2]types.PrimitiveKind) (vmValue, error) {
	resultType := left.Type
	left = m.numericOperandValue(left, numeric[0])
	right = m.numericOperandValue(right, numeric[1])
	if out, handled, err := evalIntegerBinary(operator, left, right); handled {
		if err == nil && out.scalarKind != 0 {
			out.Type = resultType
		}
		return out, err
	}
	switch operator {
	case operatorAdd:
		if isStringValue(left) || isStringValue(right) {
			if !isStringValue(left) || !isStringValue(right) || !left.Type.Equal(right.Type) {
				return vmValue{}, fmt.Errorf("operator %s requires matching String operands, got %s and %s", operator, left.Type, right.Type)
			}
			text := left.Data.(string) + right.Data.(string)
			if m.vm != nil {
				if err := m.vm.chargeAllocationBytes(int64(len(text))); err != nil {
					return vmValue{}, err
				}
			}
			return newVMValue(left.Type, text), nil
		}
		out, err := evalNumeric(operator, left, right)
		return normalizeUnaryNumericResult(resultType, out), err
	case operatorSub, operatorMul, operatorDiv, operatorMod:
		out, err := evalNumeric(operator, left, right)
		return normalizeUnaryNumericResult(resultType, out), err
	case operatorBitAnd, operatorBitOr, operatorBitXor, operatorBitClear, operatorShiftLeft, operatorShiftRight:
		out, err := evalBitwise(operator, left, right)
		return normalizeUnaryNumericResult(resultType, out), err
	case operatorComplex:
		l, err := numericAsFloat64(left)
		if err != nil {
			return vmValue{}, err
		}
		r, err := numericAsFloat64(right)
		if err != nil {
			return vmValue{}, err
		}
		resultType := "Complex128"
		if left.Type.Primitive(types.PrimitiveFloat32) && right.Type.Primitive(types.PrimitiveFloat32) {
			resultType = "Complex64"
		}
		return normalizeComplexValue(coerceRuntimeType(resultType), complex(l, r)), nil
	case operatorEqual, operatorNotEqual, operatorLess, operatorGreater, operatorLessEqual, operatorGreaterEqual:
		return m.evalCompare(operator, left, right)
	case operatorAnd, operatorOr:
		return evalLogic(operator, left, right)
	default:
		return vmValue{}, fmt.Errorf("unsupported binary operator %q", operator)
	}
}

func evalIntegerBinary(operator preparedOperator, left, right vmValue) (vmValue, bool, error) {
	if left.scalarKind == vmScalarSigned {
		l := int64(left.scalar)
		if right.scalarKind == vmScalarSigned {
			r := int64(right.scalar)
			switch operator {
			case operatorAdd:
				return normalizeSignedValue(left.Type, l+r), true, nil
			case operatorSub:
				return normalizeSignedValue(left.Type, l-r), true, nil
			case operatorMul:
				return normalizeSignedValue(left.Type, l*r), true, nil
			case operatorDiv:
				if r == 0 {
					return vmValue{}, true, newGuestPanic(errors.New("division by zero"))
				}
				return normalizeSignedValue(left.Type, l/r), true, nil
			case operatorMod:
				if r == 0 {
					return vmValue{}, true, newGuestPanic(errors.New("division by zero"))
				}
				return normalizeSignedValue(left.Type, l%r), true, nil
			case operatorBitAnd:
				return normalizeSignedValue(left.Type, l&r), true, nil
			case operatorBitOr:
				return normalizeSignedValue(left.Type, l|r), true, nil
			case operatorBitXor:
				return normalizeSignedValue(left.Type, l^r), true, nil
			case operatorBitClear:
				return normalizeSignedValue(left.Type, l&^r), true, nil
			case operatorShiftLeft, operatorShiftRight:
				if r < 0 {
					return vmValue{}, true, newGuestPanic(errors.New("negative shift amount"))
				}
				if operator == operatorShiftLeft {
					return normalizeSignedValue(left.Type, l<<uint(r)), true, nil
				}
				return normalizeSignedValue(left.Type, l>>uint(r)), true, nil
			case operatorEqual, operatorNotEqual, operatorLess, operatorGreater, operatorLessEqual, operatorGreaterEqual:
				result := l == r
				switch operator {
				case operatorNotEqual:
					result = l != r
				case operatorLess:
					result = l < r
				case operatorGreater:
					result = l > r
				case operatorLessEqual:
					result = l <= r
				case operatorGreaterEqual:
					result = l >= r
				}
				return newBoolValue(result), true, nil
			}
		}
		if right.scalarKind == vmScalarUnsigned && (operator == operatorShiftLeft || operator == operatorShiftRight) {
			r := right.scalar
			if operator == operatorShiftLeft {
				return normalizeSignedValue(left.Type, l<<uint(r)), true, nil
			}
			return normalizeSignedValue(left.Type, l>>uint(r)), true, nil
		}
	}
	if left.scalarKind == vmScalarUnsigned {
		l := left.scalar
		var r uint64
		switch right.scalarKind {
		case vmScalarUnsigned:
			r = right.scalar
		case vmScalarSigned:
			value := int64(right.scalar)
			if operator != operatorShiftLeft && operator != operatorShiftRight {
				return vmValue{}, false, nil
			}
			if value < 0 {
				return vmValue{}, true, newGuestPanic(errors.New("negative shift amount"))
			}
			r = uint64(value)
		default:
			return vmValue{}, false, nil
		}
		switch operator {
		case operatorAdd:
			return normalizeUnsignedValue(left.Type, l+r), true, nil
		case operatorSub:
			return normalizeUnsignedValue(left.Type, l-r), true, nil
		case operatorMul:
			return normalizeUnsignedValue(left.Type, l*r), true, nil
		case operatorDiv:
			if r == 0 {
				return vmValue{}, true, newGuestPanic(errors.New("division by zero"))
			}
			return normalizeUnsignedValue(left.Type, l/r), true, nil
		case operatorMod:
			if r == 0 {
				return vmValue{}, true, newGuestPanic(errors.New("division by zero"))
			}
			return normalizeUnsignedValue(left.Type, l%r), true, nil
		case operatorBitAnd:
			return normalizeUnsignedValue(left.Type, l&r), true, nil
		case operatorBitOr:
			return normalizeUnsignedValue(left.Type, l|r), true, nil
		case operatorBitXor:
			return normalizeUnsignedValue(left.Type, l^r), true, nil
		case operatorBitClear:
			return normalizeUnsignedValue(left.Type, l&^r), true, nil
		case operatorShiftLeft:
			return normalizeUnsignedValue(left.Type, l<<uint(r)), true, nil
		case operatorShiftRight:
			return normalizeUnsignedValue(left.Type, l>>uint(r)), true, nil
		case operatorEqual, operatorNotEqual, operatorLess, operatorGreater, operatorLessEqual, operatorGreaterEqual:
			result := l == r
			switch operator {
			case operatorNotEqual:
				result = l != r
			case operatorLess:
				result = l < r
			case operatorGreater:
				result = l > r
			case operatorLessEqual:
				result = l <= r
			case operatorGreaterEqual:
				result = l >= r
			}
			return newBoolValue(result), true, nil
		}
	}
	return vmValue{}, false, nil
}

func (m *moduleInstance) numericOperandValue(value vmValue, primitive types.PrimitiveKind) vmValue {
	if value.Type.Ref.Kind == types.Primitive {
		return value
	}
	if primitive != types.PrimitiveInvalid {
		value.Type = vmType{Ref: types.Builtin(primitive)}
		return value
	}
	underlyingType := value.Type.Underlying()
	if underlyingType.Ref.Kind == types.Named {
		underlyingType = m.resolvedRuntimeType(value.Type).Underlying()
	}
	if !underlyingType.Valid() || underlyingType.Equal(value.Type) {
		return value
	}
	if _, ok := underlyingType.NumericInfo(); !ok {
		return value
	}
	value.Type = underlyingType
	return value
}

func normalizeUnaryNumericResult(resultType vmType, value vmValue) vmValue {
	if resultType.Valid() && !resultType.Equal(value.Type) && !value.Type.Primitive(types.PrimitiveBool) {
		value.Type = resultType
	}
	return value
}

func evalNumeric(operator preparedOperator, left, right vmValue) (vmValue, error) {
	if isComplexValue(left) || isComplexValue(right) {
		l, err := numericAsComplex128(left)
		if err != nil {
			return vmValue{}, err
		}
		r, err := numericAsComplex128(right)
		if err != nil {
			return vmValue{}, err
		}
		target := coerceRuntimeType("Complex128")
		if isComplexValue(left) && left.Type.Valid() {
			target = left.Type
		} else if isComplexValue(right) && right.Type.Valid() {
			target = right.Type
		}
		switch operator {
		case operatorAdd:
			return normalizeComplexValue(target, l+r), nil
		case operatorSub:
			return normalizeComplexValue(target, l-r), nil
		case operatorMul:
			return normalizeComplexValue(target, l*r), nil
		case operatorDiv:
			return normalizeComplexValue(target, l/r), nil
		default:
			return vmValue{}, fmt.Errorf("operator %s requires non-complex operands", operator)
		}
	}
	if isFloatValue(left) || isFloatValue(right) {
		l, err := numericAsFloat64(left)
		if err != nil {
			return vmValue{}, err
		}
		r, err := numericAsFloat64(right)
		if err != nil {
			return vmValue{}, err
		}
		target := coerceRuntimeType("Float64")
		if isFloatValue(left) && left.Type.Valid() {
			target = left.Type
		} else if isFloatValue(right) && right.Type.Valid() {
			target = right.Type
		}
		switch operator {
		case operatorAdd:
			return normalizeFloatValue(target, l+r), nil
		case operatorSub:
			return normalizeFloatValue(target, l-r), nil
		case operatorMul:
			return normalizeFloatValue(target, l*r), nil
		case operatorDiv:
			return normalizeFloatValue(target, l/r), nil
		default:
			return vmValue{}, fmt.Errorf("operator %s requires integer operands", operator)
		}
	}
	if isUnsignedIntegerValue(left) || isUnsignedIntegerValue(right) {
		l, err := numericAsUint64(left)
		if err != nil {
			return vmValue{}, err
		}
		r, err := numericAsUint64(right)
		if err != nil {
			return vmValue{}, err
		}
		switch operator {
		case operatorAdd:
			return normalizeUnsignedValue(left.Type, l+r), nil
		case operatorSub:
			return normalizeUnsignedValue(left.Type, l-r), nil
		case operatorMul:
			return normalizeUnsignedValue(left.Type, l*r), nil
		case operatorDiv:
			if r == 0 {
				return vmValue{}, newGuestPanic(errors.New("division by zero"))
			}
			return normalizeUnsignedValue(left.Type, l/r), nil
		case operatorMod:
			if r == 0 {
				return vmValue{}, newGuestPanic(errors.New("division by zero"))
			}
			return normalizeUnsignedValue(left.Type, l%r), nil
		default:
			return vmValue{}, fmt.Errorf("unsupported numeric operator %q", operator)
		}
	}
	l, err := numericAsInt64(left)
	if err != nil {
		return vmValue{}, err
	}
	r, err := numericAsInt64(right)
	if err != nil {
		return vmValue{}, err
	}
	switch operator {
	case operatorAdd:
		return normalizeSignedValue(left.Type, l+r), nil
	case operatorSub:
		return normalizeSignedValue(left.Type, l-r), nil
	case operatorMul:
		return normalizeSignedValue(left.Type, l*r), nil
	case operatorDiv:
		if r == 0 {
			return vmValue{}, newGuestPanic(errors.New("division by zero"))
		}
		return normalizeSignedValue(left.Type, l/r), nil
	case operatorMod:
		if r == 0 {
			return vmValue{}, newGuestPanic(errors.New("division by zero"))
		}
		return normalizeSignedValue(left.Type, l%r), nil
	default:
		return vmValue{}, fmt.Errorf("unsupported numeric operator %q", operator)
	}
}

func evalBitwise(operator preparedOperator, left, right vmValue) (vmValue, error) {
	if !isIntegerValue(left) || !isIntegerValue(right) {
		return vmValue{}, fmt.Errorf("operator %s expects integer operands", operator)
	}
	if isUnsignedIntegerValue(left) {
		l, err := numericAsUint64(left)
		if err != nil {
			return vmValue{}, err
		}
		r, err := numericAsUint64(right)
		if operator == operatorShiftLeft || operator == operatorShiftRight {
			r, err = numericShiftCount(right)
		}
		if err != nil {
			return vmValue{}, err
		}
		switch operator {
		case operatorBitAnd:
			return normalizeUnsignedValue(left.Type, l&r), nil
		case operatorBitOr:
			return normalizeUnsignedValue(left.Type, l|r), nil
		case operatorBitXor:
			return normalizeUnsignedValue(left.Type, l^r), nil
		case operatorBitClear:
			return normalizeUnsignedValue(left.Type, l&^r), nil
		case operatorShiftLeft:
			return normalizeUnsignedValue(left.Type, l<<uint(r)), nil
		case operatorShiftRight:
			return normalizeUnsignedValue(left.Type, l>>uint(r)), nil
		default:
			return vmValue{}, fmt.Errorf("unsupported bitwise operator %q", operator)
		}
	}
	l, err := numericAsInt64(left)
	if err != nil {
		return vmValue{}, err
	}
	r, err := numericAsUint64(right)
	if operator == operatorShiftLeft || operator == operatorShiftRight {
		r, err = numericShiftCount(right)
	}
	if err != nil {
		return vmValue{}, err
	}
	switch operator {
	case operatorBitAnd:
		return normalizeSignedValue(left.Type, l&int64(r)), nil
	case operatorBitOr:
		return normalizeSignedValue(left.Type, l|int64(r)), nil
	case operatorBitXor:
		return normalizeSignedValue(left.Type, l^int64(r)), nil
	case operatorBitClear:
		return normalizeSignedValue(left.Type, l&^int64(r)), nil
	case operatorShiftLeft:
		return normalizeSignedValue(left.Type, l<<uint(r)), nil
	case operatorShiftRight:
		return normalizeSignedValue(left.Type, l>>uint(r)), nil
	default:
		return vmValue{}, fmt.Errorf("unsupported bitwise operator %q", operator)
	}
}

func numericShiftCount(value vmValue) (uint64, error) {
	if count, ok := value.signedValue(); ok && count < 0 {
		return 0, newGuestPanic(errors.New("negative shift amount"))
	}
	return numericAsUint64(value)
}
