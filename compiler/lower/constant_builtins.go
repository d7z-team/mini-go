package lower

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) constBuiltinCallValue(expr ast.Expression, scope *funcScope) (json.RawMessage, string, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
		return nil, "", false
	}
	switch strings.TrimSpace(expr.Callee.Name) {
	case "len", "cap":
		if len(expr.Args) != 1 {
			return nil, "", false
		}
		arg := expr.Args[0]
		if check.ContainsNonConstantLenOperand(arg) {
			return nil, "", false
		}
		if raw, typ, ok := l.constValue(arg, scope); ok {
			if text, ok := l.constString(raw, typ); ok {
				if expr.Callee.Name == "cap" {
					return nil, "", false
				}
				return json.RawMessage(strconv.FormatInt(int64(len([]byte(text))), 10)), "Int", true
			}
		}
		typ := l.resolveSourceTypePtr(arg.Type, nil)
		if typ == "" || typ == "Any" {
			typ = l.expressionType(arg, scope)
		}
		if array, ok := l.pointerArrayType(typ); ok {
			typ = array
		}
		length, _, ok := l.arrayTypeInfo(typ)
		if !ok {
			return nil, "", false
		}
		if length < 0 {
			return nil, "", false
		}
		return json.RawMessage(strconv.FormatInt(length, 10)), "Int", true
	case "complex":
		if len(expr.Args) != 2 {
			return nil, "", false
		}
		leftRaw, leftType, ok := l.constValue(expr.Args[0], scope)
		if !ok {
			return nil, "", false
		}
		rightRaw, rightType, ok := l.constValue(expr.Args[1], scope)
		if !ok {
			return nil, "", false
		}
		left, ok := l.constExactRational(leftRaw, leftType)
		if !ok {
			return nil, "", false
		}
		right, ok := l.constExactRational(rightRaw, rightType)
		if !ok {
			return nil, "", false
		}
		typ := "Complex128"
		if l.underlyingConstType(leftType) == "Float32" && l.underlyingConstType(rightType) == "Float32" {
			typ = "Complex64"
		}
		out, ok := l.complexRawForType(exactComplex{realPart: left, imaginaryPart: right}, typ, l.untypedConstExpression(expr, scope))
		return out, typ, ok
	case "real", "imag":
		if len(expr.Args) != 1 {
			return nil, "", false
		}
		raw, typ, ok := l.constValue(expr.Args[0], scope)
		if !ok {
			return nil, "", false
		}
		value, ok := l.constExactComplex(raw, typ)
		if !ok {
			return nil, "", false
		}
		out := value.realPart
		if strings.TrimSpace(expr.Callee.Name) == "imag" {
			out = value.imaginaryPart
		}
		resultType := "Float64"
		if l.underlyingConstType(typ) == "Complex64" {
			resultType = "Float32"
		}
		raw, ok = l.rationalRawForType(out, resultType, l.untypedConstExpression(expr, scope))
		return raw, resultType, ok
	case "min", "max":
		return l.constMinMaxValue(expr, scope)
	default:
		return nil, "", false
	}
}

func (l *lowerer) constMinMaxValue(expr ast.Expression, scope *funcScope) (json.RawMessage, string, bool) {
	if len(expr.Args) == 0 || expr.Ellipsis {
		return nil, "", false
	}
	target, ok := l.minMaxTargetType(expr, scope)
	if !ok {
		return nil, "", false
	}
	values := make([]constantValue, 0, len(expr.Args))
	untyped := true
	for _, arg := range expr.Args {
		if !l.untypedConstExpression(arg, scope) {
			untyped = false
			break
		}
	}
	for _, arg := range expr.Args {
		raw, typ, ok := l.constValue(arg, scope)
		if !ok {
			return nil, "", false
		}
		if !untyped && typ != target {
			raw, typ, ok = l.convertConstValue(raw, typ, target)
			if !ok {
				return nil, "", false
			}
		}
		values = append(values, constantValue{Type: typ, Value: raw})
	}
	best := 0
	for i := 1; i < len(values); i++ {
		operator := "<"
		if strings.TrimSpace(expr.Callee.Name) == "max" {
			operator = ">"
		}
		if better, ok := l.constOrderedCompare(operator, values[i], values[best]); !ok {
			return nil, "", false
		} else if better {
			best = i
		}
	}
	selected := values[best]
	if untyped && isFloatType(l.underlyingConstType(target)) {
		value, ok := l.constExactRational(selected.Value, selected.Type)
		if !ok {
			return nil, "", false
		}
		raw, ok := exactRationalRaw(value)
		return raw, target, ok
	}
	return append(json.RawMessage(nil), selected.Value...), selected.Type, true
}

func (l *lowerer) constOrderedCompare(operator string, left, right constantValue) (bool, bool) {
	leftType := l.underlyingConstType(left.Type)
	rightType := l.underlyingConstType(right.Type)
	if leftType == "String" && rightType == "String" {
		leftText, leftOK := l.constString(left.Value, left.Type)
		rightText, rightOK := l.constString(right.Value, right.Type)
		if !leftOK || !rightOK {
			return false, false
		}
		if operator == "<" {
			return leftText < rightText, true
		}
		return leftText > rightText, true
	}
	if isIntegerType(leftType) && isIntegerType(rightType) {
		leftInt, leftOK := l.constExactInteger(left.Value, left.Type)
		rightInt, rightOK := l.constExactInteger(right.Value, right.Type)
		if !leftOK || !rightOK {
			return false, false
		}
		cmp := constant.CompareSignedDecimal(leftInt, rightInt)
		if operator == "<" {
			return cmp < 0, true
		}
		return cmp > 0, true
	}
	if (isIntegerType(leftType) || isFloatType(leftType)) && (isIntegerType(rightType) || isFloatType(rightType)) {
		leftFloat, leftOK := l.constExactRational(left.Value, left.Type)
		rightFloat, rightOK := l.constExactRational(right.Value, right.Type)
		if !leftOK || !rightOK {
			return false, false
		}
		comparison := constant.CompareRational(leftFloat, rightFloat)
		if operator == "<" {
			return comparison < 0, true
		}
		return comparison > 0, true
	}
	return false, false
}

func normalizeConstUint(value uint64, typ string) uint64 {
	switch typ {
	case "Uint8":
		return uint64(uint8(value))
	case "Uint16":
		return uint64(uint16(value))
	case "Uint32":
		return uint64(uint32(value))
	default:
		return value
	}
}

func complexRaw(value complex128) (json.RawMessage, bool) {
	raw, err := json.Marshal(struct {
		Real float64 `json:"real"`
		Imag float64 `json:"imag"`
	}{Real: real(value), Imag: imag(value)})
	if err != nil {
		return nil, false
	}
	return json.RawMessage(raw), true
}

func (l *lowerer) underlyingConstType(typ string) string {
	typ = strings.TrimSpace(typ)
	seen := map[string]struct{}{}
	for i := 0; i < 32; i++ {
		if typ == "" {
			return ""
		}
		if _, ok := seen[typ]; ok {
			return typ
		}
		seen[typ] = struct{}{}
		if alias, ok := l.typeAliases[typ]; ok && strings.TrimSpace(alias) != "" {
			typ = strings.TrimSpace(alias)
			continue
		}
		if decl, ok := l.typeDecls[typ]; ok {
			next := l.resolveSourceType(decl)
			if strings.TrimSpace(next) != "" && next != typ {
				typ = next
				continue
			}
		}
		if export, ok := l.importedTypeInfo(typ); ok {
			next := l.resolveType(export.Underlying)
			if strings.TrimSpace(next) != "" && next != typ {
				typ = next
				continue
			}
		}
		return typ
	}
	return typ
}
