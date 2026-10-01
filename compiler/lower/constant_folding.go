package lower

import (
	"math"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/constant"
)

func foldInt64Unary(operator string, value int64) (int64, bool) {
	switch operator {
	case "+":
		return value, true
	case "-":
		return -value, true
	case "^":
		return ^value, true
	default:
		return 0, false
	}
}

func foldInt64Binary(operator string, left, right int64) (int64, bool) {
	switch operator {
	case "+":
		return left + right, true
	case "-":
		return left - right, true
	case "*":
		return left * right, true
	case "/":
		if right == 0 {
			return 0, false
		}
		return left / right, true
	case "%":
		if right == 0 {
			return 0, false
		}
		return left % right, true
	case "&":
		return left & right, true
	case "|":
		return left | right, true
	case "^":
		return left ^ right, true
	case "<<":
		if right < 0 {
			return 0, false
		}
		return left << uint(right), true
	case ">>":
		if right < 0 {
			return 0, false
		}
		return left >> uint(right), true
	default:
		return 0, false
	}
}

func floatConstant(value float64) (*constant.Value, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false
	}
	return constant.Scalar(strconv.FormatFloat(value, 'g', -1, 64)), true
}

func foldBoolUnary(operator string, raw *constant.Value, typ string) (*constant.Value, bool) {
	value, ok := constBool(raw, typ)
	if !ok {
		return nil, false
	}
	switch operator {
	case "!":
		return booleanConstant(!value), true
	default:
		return nil, false
	}
}

func foldBoolBinary(operator string, leftRaw *constant.Value, leftType string, rightRaw *constant.Value, rightType string) (*constant.Value, string, bool) {
	left, ok := constBool(leftRaw, leftType)
	if !ok {
		return nil, "", false
	}
	right, ok := constBool(rightRaw, rightType)
	if !ok {
		return nil, "", false
	}
	switch operator {
	case "&&":
		return booleanConstant(left && right), "Bool", true
	case "||":
		return booleanConstant(left || right), "Bool", true
	case "==":
		return booleanConstant(left == right), "Bool", true
	case "!=":
		return booleanConstant(left != right), "Bool", true
	default:
		return nil, "", false
	}
}

func constBool(value *constant.Value, typ string) (bool, bool) {
	if value == nil || typ != "Bool" || value.Kind() != constant.Boolean {
		return false, false
	}
	return value.Text == "true", true
}

func (l *lowerer) constBool(raw *constant.Value, typ string) (bool, bool) {
	if out, ok := constBool(raw, typ); ok {
		return out, true
	}
	underlying := l.underlyingConstType(typ)
	if underlying == typ {
		return false, false
	}
	return constBool(raw, underlying)
}

func booleanConstant(value bool) *constant.Value {
	if value {
		return constant.Scalar("true")
	}
	return constant.Scalar("false")
}

func foldInt64Compare(operator string, left, right int64) (*constant.Value, bool) {
	switch operator {
	case "==":
		return booleanConstant(left == right), true
	case "!=":
		return booleanConstant(left != right), true
	case "<":
		return booleanConstant(left < right), true
	case "<=":
		return booleanConstant(left <= right), true
	case ">":
		return booleanConstant(left > right), true
	case ">=":
		return booleanConstant(left >= right), true
	default:
		return nil, false
	}
}

func foldExactIntegerCompare(operator, left, right string) (*constant.Value, bool) {
	if _, ok := constant.NormalizeSignedDecimal(left); !ok {
		return nil, false
	}
	if _, ok := constant.NormalizeSignedDecimal(right); !ok {
		return nil, false
	}
	cmp := constant.CompareSignedDecimal(left, right)
	switch operator {
	case "==":
		return booleanConstant(cmp == 0), true
	case "!=":
		return booleanConstant(cmp != 0), true
	case "<":
		return booleanConstant(cmp < 0), true
	case "<=":
		return booleanConstant(cmp <= 0), true
	case ">":
		return booleanConstant(cmp > 0), true
	case ">=":
		return booleanConstant(cmp >= 0), true
	default:
		return nil, false
	}
}

func foldExactIntegerBinary(operator, left, right string) (string, bool) {
	leftValue, leftOK := constant.Integer(left, "Int", true)
	rightValue, rightOK := constant.Integer(right, "Int", true)
	if !leftOK || !rightOK {
		return "", false
	}
	value, ok := constant.Binary(operator, leftValue, rightValue)
	return value.Text, ok
}

func (l *lowerer) foldExactIntegerUnary(operator, value, typ string) (string, bool) {
	value, ok := constant.NormalizeSignedDecimal(value)
	if !ok {
		return "", false
	}
	switch operator {
	case "+":
		return value, true
	case "-":
		return constant.NegateSignedDecimal(value)
	case "^":
		kind := l.underlyingConstType(typ)
		if isUnsignedIntegerType(kind) {
			info, ok := numericTypeInfo(kind)
			if !ok || strings.HasPrefix(value, "-") {
				return "", false
			}
			upper := constant.UnsignedIntegerMax(info.Bits)
			return constant.SubtractUnsignedDecimal(upper, value)
		}
		negated, ok := constant.NegateSignedDecimal(value)
		if !ok {
			return "", false
		}
		return constant.SubtractSignedDecimal(negated, "1")
	default:
		return "", false
	}
}

func foldStringBinary(operator string, leftRaw *constant.Value, leftType string, rightRaw *constant.Value, rightType string) (*constant.Value, string, bool) {
	left, ok := constString(leftRaw, leftType)
	if !ok {
		return nil, "", false
	}
	right, ok := constString(rightRaw, rightType)
	if !ok {
		return nil, "", false
	}
	switch operator {
	case "+":
		return stringConstant(left + right), "String", true
	case "==":
		return booleanConstant(left == right), "Bool", true
	case "!=":
		return booleanConstant(left != right), "Bool", true
	case "<":
		return booleanConstant(left < right), "Bool", true
	case "<=":
		return booleanConstant(left <= right), "Bool", true
	case ">":
		return booleanConstant(left > right), "Bool", true
	case ">=":
		return booleanConstant(left >= right), "Bool", true
	default:
		return nil, "", false
	}
}

func constString(value *constant.Value, typ string) (string, bool) {
	if value == nil || typ != "String" {
		return "", false
	}
	return value.StringValue()
}

func stringConstant(text string) *constant.Value {
	return constant.String(text, "String", false).Ref()
}

func (l *lowerer) constString(raw *constant.Value, typ string) (string, bool) {
	if out, ok := constString(raw, typ); ok {
		return out, true
	}
	underlying := l.underlyingConstType(typ)
	if underlying == typ {
		return "", false
	}
	return constString(raw, underlying)
}
