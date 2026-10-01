package lower

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/runtime/bytecode"
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

func finiteFloatRaw(value float64) (json.RawMessage, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false
	}
	return json.RawMessage(strconv.FormatFloat(value, 'g', -1, 64)), true
}

func foldBoolUnary(operator string, raw json.RawMessage, typ string) (json.RawMessage, bool) {
	value, ok := constBool(raw, typ)
	if !ok {
		return nil, false
	}
	switch operator {
	case "!":
		return boolRaw(!value), true
	default:
		return nil, false
	}
}

func foldBoolBinary(operator string, leftRaw json.RawMessage, leftType string, rightRaw json.RawMessage, rightType string) (json.RawMessage, string, bool) {
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
		return boolRaw(left && right), "Bool", true
	case "||":
		return boolRaw(left || right), "Bool", true
	case "==":
		return boolRaw(left == right), "Bool", true
	case "!=":
		return boolRaw(left != right), "Bool", true
	default:
		return nil, "", false
	}
}

func constBool(raw json.RawMessage, typ string) (bool, bool) {
	if typ != "Bool" {
		return false, false
	}
	var out bool
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, false
	}
	return out, true
}

func (l *lowerer) constBool(raw json.RawMessage, typ string) (bool, bool) {
	if out, ok := constBool(raw, typ); ok {
		return out, true
	}
	underlying := l.underlyingConstType(typ)
	if underlying == typ {
		return false, false
	}
	return constBool(raw, underlying)
}

func boolRaw(value bool) json.RawMessage {
	if value {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

func foldInt64Compare(operator string, left, right int64) (json.RawMessage, bool) {
	switch operator {
	case "==":
		return boolRaw(left == right), true
	case "!=":
		return boolRaw(left != right), true
	case "<":
		return boolRaw(left < right), true
	case "<=":
		return boolRaw(left <= right), true
	case ">":
		return boolRaw(left > right), true
	case ">=":
		return boolRaw(left >= right), true
	default:
		return nil, false
	}
}

func foldExactIntegerCompare(operator, left, right string) (json.RawMessage, bool) {
	if _, ok := constant.NormalizeSignedDecimal(left); !ok {
		return nil, false
	}
	if _, ok := constant.NormalizeSignedDecimal(right); !ok {
		return nil, false
	}
	cmp := constant.CompareSignedDecimal(left, right)
	switch operator {
	case "==":
		return boolRaw(cmp == 0), true
	case "!=":
		return boolRaw(cmp != 0), true
	case "<":
		return boolRaw(cmp < 0), true
	case "<=":
		return boolRaw(cmp <= 0), true
	case ">":
		return boolRaw(cmp > 0), true
	case ">=":
		return boolRaw(cmp >= 0), true
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

func foldStringBinary(operator string, leftRaw json.RawMessage, leftType string, rightRaw json.RawMessage, rightType string) (json.RawMessage, string, bool) {
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
		return bytecode.EncodeStringConstant(left + right), "String", true
	case "==":
		return boolRaw(left == right), "Bool", true
	case "!=":
		return boolRaw(left != right), "Bool", true
	case "<":
		return boolRaw(left < right), "Bool", true
	case "<=":
		return boolRaw(left <= right), "Bool", true
	case ">":
		return boolRaw(left > right), "Bool", true
	case ">=":
		return boolRaw(left >= right), "Bool", true
	default:
		return nil, "", false
	}
}

func constString(raw json.RawMessage, typ string) (string, bool) {
	if typ != "String" {
		return "", false
	}
	out, err := bytecode.DecodeStringConstant(raw)
	return out, err == nil
}

func (l *lowerer) constString(raw json.RawMessage, typ string) (string, bool) {
	if out, ok := constString(raw, typ); ok {
		return out, true
	}
	underlying := l.underlyingConstType(typ)
	if underlying == typ {
		return "", false
	}
	return constString(raw, underlying)
}
