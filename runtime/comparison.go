package runtime

import "fmt"

func evalLogic(operator preparedOperator, left, right vmValue) (vmValue, error) {
	l, err := truthy(left)
	if err != nil {
		return vmValue{}, err
	}
	r, err := truthy(right)
	if err != nil {
		return vmValue{}, err
	}
	switch operator {
	case operatorAnd:
		return newBoolValue(l && r), nil
	case operatorOr:
		return newBoolValue(l || r), nil
	default:
		return vmValue{}, fmt.Errorf("unsupported logic operator %q", operator)
	}
}

func compareFloat(operator preparedOperator, left, right float64) bool {
	switch operator {
	case operatorEqual:
		return left == right
	case operatorNotEqual:
		return left != right
	case operatorLess:
		return left < right
	case operatorGreater:
		return left > right
	case operatorLessEqual:
		return left <= right
	case operatorGreaterEqual:
		return left >= right
	default:
		return false
	}
}

func compareString(operator preparedOperator, left, right string) bool {
	switch operator {
	case operatorEqual:
		return left == right
	case operatorNotEqual:
		return left != right
	case operatorLess:
		return left < right
	case operatorGreater:
		return left > right
	case operatorLessEqual:
		return left <= right
	case operatorGreaterEqual:
		return left >= right
	default:
		return false
	}
}

func isStringValue(value vmValue) bool {
	_, ok := value.Data.(string)
	return ok
}

func asInt64(value vmValue) (int64, error) {
	if !isIntegerValue(value) {
		return 0, fmt.Errorf("expected integer value, got %s data=%#v", value.Type, value.Data)
	}
	return numericAsInt64(value)
}

func equalNumericValues(left, right vmValue) (bool, error) {
	if isComplexValue(left) || isComplexValue(right) {
		l, err := numericAsComplex128(left)
		if err != nil {
			return false, err
		}
		r, err := numericAsComplex128(right)
		if err != nil {
			return false, err
		}
		return l == r, nil
	}
	if isFloatValue(left) || isFloatValue(right) {
		l, err := numericAsFloat64(left)
		if err != nil {
			return false, err
		}
		r, err := numericAsFloat64(right)
		if err != nil {
			return false, err
		}
		return l == r, nil
	}
	comparison, ok, err := compareIntegerValues(left, right)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("invalid integer operands %s and %s", left.Type, right.Type)
	}
	return comparison == 0, nil
}

const (
	signedIntegerKind   = 1
	unsignedIntegerKind = 2
)

func integerKind(value vmValue) int {
	if isSignedIntegerValue(value) {
		return signedIntegerKind
	}
	if isUnsignedIntegerValue(value) {
		return unsignedIntegerKind
	}
	return 0
}

func compareIntegerValues(left, right vmValue) (int, bool, error) {
	leftKind := integerKind(left)
	rightKind := integerKind(right)
	if leftKind == 0 || rightKind == 0 {
		return 0, false, nil
	}
	if leftKind == signedIntegerKind && rightKind == signedIntegerKind {
		l, err := numericAsInt64(left)
		if err != nil {
			return 0, true, err
		}
		r, err := numericAsInt64(right)
		if err != nil {
			return 0, true, err
		}
		return compareInt64(l, r), true, nil
	}
	if leftKind == unsignedIntegerKind && rightKind == unsignedIntegerKind {
		l, err := numericAsUint64(left)
		if err != nil {
			return 0, true, err
		}
		r, err := numericAsUint64(right)
		if err != nil {
			return 0, true, err
		}
		return compareUint64(l, r), true, nil
	}
	if leftKind == signedIntegerKind {
		l, err := numericAsInt64(left)
		if err != nil {
			return 0, true, err
		}
		r, err := numericAsUint64(right)
		if err != nil {
			return 0, true, err
		}
		if l < 0 {
			return -1, true, nil
		}
		return compareUint64(uint64(l), r), true, nil
	}
	l, err := numericAsUint64(left)
	if err != nil {
		return 0, true, err
	}
	r, err := numericAsInt64(right)
	if err != nil {
		return 0, true, err
	}
	if r < 0 {
		return 1, true, nil
	}
	return compareUint64(l, uint64(r)), true, nil
}

func compareNumericValues(operator preparedOperator, left, right vmValue) (bool, error) {
	if comparison, ok, err := compareIntegerValues(left, right); ok {
		if err != nil {
			return false, err
		}
		return compareOrdering(operator, comparison), nil
	}
	l, err := numericAsFloat64(left)
	if err != nil {
		return false, err
	}
	r, err := numericAsFloat64(right)
	if err != nil {
		return false, err
	}
	return compareFloat(operator, l, r), nil
}

func compareOrdering(operator preparedOperator, comparison int) bool {
	switch operator {
	case operatorLess:
		return comparison < 0
	case operatorGreater:
		return comparison > 0
	case operatorLessEqual:
		return comparison <= 0
	case operatorGreaterEqual:
		return comparison >= 0
	case operatorEqual:
		return comparison == 0
	case operatorNotEqual:
		return comparison != 0
	default:
		return false
	}
}

func compareInt64(left, right int64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareUint64(left, right uint64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
