package lower

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

func (l *lowerer) convertConstValue(raw json.RawMessage, sourceType, target string) (json.RawMessage, string, bool) {
	if target == "Any" {
		// Interface conversion must retain the operand's dynamic type at runtime.
		return nil, "", false
	}
	targetKind := l.underlyingConstType(target)
	if targetKind == "" {
		targetKind = target
	}
	switch targetKind {
	case "Bool":
		value, ok := l.constBool(raw, sourceType)
		if !ok {
			return nil, "", false
		}
		return boolRaw(value), target, true
	case "String":
		if value, ok := l.constString(raw, sourceType); ok {
			return bytecode.EncodeStringConstant(value), target, true
		}
		value, ok := l.constExactInteger(raw, sourceType)
		if !ok {
			return nil, "", false
		}
		r := utf8.RuneError
		if candidate, ok := constant.SignedDecimalInt64(value); ok {
			if candidate >= 0 && candidate <= utf8.MaxRune {
				r = rune(candidate)
			}
		}
		return bytecode.EncodeStringConstant(string(r)), target, true
	case "Float32", "Float64":
		value, ok := l.constExactRational(raw, sourceType)
		if !ok {
			return nil, "", false
		}
		out, ok := l.rationalRawForType(value, targetKind, false)
		return out, target, ok
	case "Complex64", "Complex128":
		value, ok := l.constExactComplex(raw, sourceType)
		if !ok {
			return nil, "", false
		}
		out, ok := l.complexRawForType(value, targetKind, false)
		return out, target, ok
	default:
		if isUnsignedIntegerType(targetKind) {
			value, ok := l.constExactInteger(raw, sourceType)
			if !ok {
				rational, rationalOK := l.constExactRational(raw, sourceType)
				if !rationalOK {
					return nil, "", false
				}
				value, ok = rational.Integer()
				if !ok {
					return nil, "", false
				}
			}
			if !l.constRepresentableAsUnsignedInteger(value, targetKind) {
				return nil, "", false
			}
			encoded, err := json.Marshal(strings.TrimPrefix(value, "+"))
			if err != nil {
				return nil, "", false
			}
			return json.RawMessage(encoded), target, true
		}
		if isSignedIntegerType(targetKind) {
			value, ok := l.constExactInteger(raw, sourceType)
			if !ok {
				rational, rationalOK := l.constExactRational(raw, sourceType)
				if !rationalOK {
					return nil, "", false
				}
				value, ok = rational.Integer()
				if !ok {
					return nil, "", false
				}
			}
			if !l.constRepresentableAsSignedInteger(value, targetKind) {
				return nil, "", false
			}
			raw, ok := l.exactIntegerRawForType(value, target)
			return raw, target, ok
		}
		if sourceType == target {
			return append(json.RawMessage(nil), raw...), target, true
		}
		return nil, "", false
	}
}

func (l *lowerer) exactIntegerRawForType(value, typ string) (json.RawMessage, bool) {
	value, ok := constant.NormalizeSignedDecimal(value)
	if !ok {
		return nil, false
	}
	kind := l.underlyingConstType(typ)
	if isUnsignedIntegerType(kind) {
		if strings.HasPrefix(value, "-") {
			return nil, false
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		return json.RawMessage(encoded), true
	}
	if _, ok := constant.SignedDecimalInt64(value); ok {
		return json.RawMessage(value), true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(encoded), true
}

func (l *lowerer) constRepresentableAsSignedInteger(value, target string) bool {
	info, ok := numericTypeInfo(l.underlyingConstType(target))
	if !ok || info.Kind != types.NumericSigned {
		return false
	}
	lower, upper := constant.SignedIntegerBounds(info.Bits)
	return constant.CompareSignedDecimal(value, lower) >= 0 && constant.CompareSignedDecimal(value, upper) <= 0
}

func (l *lowerer) constRepresentableAsUnsignedInteger(value, target string) bool {
	info, ok := numericTypeInfo(l.underlyingConstType(target))
	if !ok || info.Kind != types.NumericUnsigned {
		return false
	}
	value, ok = constant.NormalizeSignedDecimal(value)
	if !ok || strings.HasPrefix(value, "-") {
		return false
	}
	return constant.CompareUnsignedDecimal(value, constant.UnsignedIntegerMax(info.Bits)) <= 0
}

func (l *lowerer) constIntegerRepresentable(value, target string) bool {
	kind := l.underlyingConstType(target)
	if isUnsignedIntegerType(kind) {
		return l.constRepresentableAsUnsignedInteger(value, kind)
	}
	return isSignedIntegerType(kind) && l.constRepresentableAsSignedInteger(value, kind)
}
