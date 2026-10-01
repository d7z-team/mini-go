package lower

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) untypedConstExpression(expr ast.Expression, scope *funcScope) bool {
	if untyped, ok := l.semanticUntypedConstant(expr); ok {
		return untyped
	}
	switch expr.Kind {
	case ast.ExprLiteral:
		return true
	case ast.ExprIdent:
		value, ok := l.lookupConstValue(expr.Name, scope)
		return ok && value.Untyped
	case ast.ExprSelector:
		export, ok := l.selectorExport(expr, scope)
		return ok && export.Kind == check.ObjectConst && export.Untyped
	case ast.ExprUnary:
		return expr.Operand != nil && l.untypedConstExpression(*expr.Operand, scope)
	case ast.ExprBinary:
		return expr.Left != nil && expr.Right != nil && l.untypedConstExpression(*expr.Left, scope) && l.untypedConstExpression(*expr.Right, scope)
	case ast.ExprCall:
		if _, ok := l.typeConversionCallTarget(expr, scope); ok {
			return false
		}
		if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
			return false
		}
		if !l.isBuiltinCallName(expr.Callee.Name, expr.Callee.Span, scope) {
			return false
		}
		switch strings.TrimSpace(expr.Callee.Name) {
		case "len", "cap":
			_, _, ok := l.constBuiltinCallValue(expr, scope)
			return ok
		case "complex":
			return len(expr.Args) == 2 && l.untypedConstExpression(expr.Args[0], scope) && l.untypedConstExpression(expr.Args[1], scope)
		case "real", "imag":
			return len(expr.Args) == 1 && l.untypedConstExpression(expr.Args[0], scope)
		case "min", "max":
			if len(expr.Args) == 0 || expr.Ellipsis {
				return false
			}
			for _, arg := range expr.Args {
				if !l.untypedConstExpression(arg, scope) {
					return false
				}
			}
			return true
		}
	default:
		return false
	}
	return false
}

func (l *lowerer) constValue(expr ast.Expression, scope *funcScope) (json.RawMessage, string, bool) {
	if l.semantic != nil {
		if value, ok := l.semantic.Constants[expr.NodeID]; ok {
			l.markConstantImports(expr, scope)
			typ := value.Type
			if ref, err := l.typeParser.Parse(typ); err == nil {
				typ = l.formatSemanticType(ref)
			}
			kind := l.underlyingConstType(typ)
			if value.Imag != "" {
				realPart, _ := constant.ParseRationalLiteral(value.Real)
				imaginaryPart, _ := constant.ParseRationalLiteral(value.Imag)
				raw, valid := l.complexRawForType(exactComplex{realPart: realPart, imaginaryPart: imaginaryPart}, typ, value.Untyped)
				return raw, typ, valid
			}
			if kind == "Float32" || kind == "Float64" {
				rational, _ := value.Rational()
				raw, valid := l.rationalRawForType(rational, typ, value.Untyped)
				return raw, typ, valid
			}
			if !value.Untyped {
				return l.convertConstValue(value.JSON(), typ, typ)
			}
			return value.JSON(), typ, true
		}
	}
	switch expr.Kind {
	case ast.ExprLiteral:
		return literalValue(expr)
	case ast.ExprIdent:
		if value, ok := l.lookupConstValue(expr.Name, scope); ok {
			return append(json.RawMessage(nil), value.Value...), value.Type, true
		}
		if export, ok := l.dotImportExport(expr.Name, expr.Span); ok && export.Kind == check.ObjectConst && len(export.Value) != 0 {
			return append(json.RawMessage(nil), export.Value...), export.Type, true
		}
		return nil, "", false
	case ast.ExprSelector:
		if export, ok := l.selectorExport(expr, scope); ok && export.Kind == check.ObjectConst && len(export.Value) != 0 {
			l.markImportExport(export.ModulePath, expr.Field)
			return append(json.RawMessage(nil), export.Value...), export.Type, true
		}
		return nil, "", false
	case ast.ExprUnary:
		raw, typ, ok := l.constValue(derefExpr(expr.Operand), scope)
		if !ok {
			return nil, "", false
		}
		if expr.Operand != nil && l.untypedConstExpression(*expr.Operand, scope) && isNumericType(l.underlyingConstType(typ)) {
			typ = defaultUntypedConstType(typ, raw)
		}
		if typ == "" || typ == "Any" {
			typ = inferConstRawDefaultType(raw)
		}
		if out, ok := foldBoolUnary(expr.Operator, raw, typ); ok {
			return out, "Bool", true
		}
		if value, ok := l.constExactComplex(raw, typ); ok && isComplexType(l.underlyingConstType(typ)) {
			switch expr.Operator {
			case "+":
				untyped := l.untypedConstExpression(expr, scope)
				out, ok := l.complexRawForType(value, typ, untyped)
				if !ok && !untyped {
					l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
				}
				return out, typ, ok
			case "-":
				value.realPart, _ = constant.NegateRational(value.realPart)
				value.imaginaryPart, _ = constant.NegateRational(value.imaginaryPart)
				untyped := l.untypedConstExpression(expr, scope)
				out, ok := l.complexRawForType(value, typ, untyped)
				if !ok && !untyped {
					l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
				}
				return out, typ, ok
			}
		}
		if value, ok := l.constExactRational(raw, typ); ok && isFloatType(l.underlyingConstType(typ)) {
			switch expr.Operator {
			case "+":
				untyped := l.untypedConstExpression(expr, scope)
				if !untyped && value.IsZero() && constantRawNegativeZero(raw) {
					return json.RawMessage("-0"), typ, true
				}
				out, ok := l.rationalRawForType(value, typ, untyped)
				if !ok && !untyped {
					l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
				}
				return out, typ, ok
			case "-":
				negativeZero := value.IsZero() && constantRawNegativeZero(raw)
				value, ok = constant.NegateRational(value)
				if !ok {
					return nil, "", false
				}
				untyped := l.untypedConstExpression(expr, scope)
				if !untyped && value.IsZero() {
					if negativeZero {
						return json.RawMessage("0"), typ, true
					}
					return json.RawMessage("-0"), typ, true
				}
				out, ok := l.rationalRawForType(value, typ, untyped)
				if !ok && !untyped {
					l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
				}
				return out, typ, ok
			}
		}
		if out, ok := l.foldUnsignedIntegerUnary(expr.Operator, raw, typ); ok {
			return out, typ, true
		}
		if value, ok := l.constExactInteger(raw, typ); ok {
			out, ok := l.foldExactIntegerUnary(expr.Operator, value, typ)
			if ok {
				if !l.untypedConstExpression(expr, scope) && !l.constIntegerRepresentable(out, typ) {
					l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
					return nil, "", false
				}
				raw, ok := l.exactIntegerRawForType(out, typ)
				return raw, typ, ok
			}
		}
		value, ok := l.constInt64(raw, typ)
		if !ok {
			return nil, "", false
		}
		out, ok := foldInt64Unary(expr.Operator, value)
		if !ok {
			return nil, "", false
		}
		return json.RawMessage(strconv.FormatInt(out, 10)), typ, true
	case ast.ExprBinary:
		if !l.validateConstantArithmetic(expr, scope) {
			return nil, "", false
		}
		leftRaw, leftType, ok := l.constValue(derefExpr(expr.Left), scope)
		if !ok {
			return nil, "", false
		}
		rightRaw, rightType, ok := l.constValue(derefExpr(expr.Right), scope)
		if !ok {
			return nil, "", false
		}
		if leftType == "" || leftType == "Any" {
			leftType = inferConstRawDefaultType(leftRaw)
		}
		if rightType == "" || rightType == "Any" {
			rightType = inferConstRawDefaultType(rightRaw)
		}
		leftUntyped := expr.Left != nil && l.untypedConstExpression(*expr.Left, scope)
		rightUntyped := expr.Right != nil && l.untypedConstExpression(*expr.Right, scope)
		if out, typ, ok := foldBoolBinary(expr.Operator, leftRaw, leftType, rightRaw, rightType); ok {
			return out, typ, true
		}
		if out, typ, ok := foldStringBinary(expr.Operator, leftRaw, leftType, rightRaw, rightType); ok {
			return out, typ, true
		}
		if leftValue, ok := l.constExactInteger(leftRaw, leftType); ok {
			if rightValue, ok := l.constExactInteger(rightRaw, rightType); ok {
				if out, ok := foldExactIntegerCompare(expr.Operator, leftValue, rightValue); ok {
					return out, "Bool", true
				}
				out, ok := foldExactIntegerBinary(expr.Operator, leftValue, rightValue)
				if ok {
					resultType := leftType
					if leftUntyped && !rightUntyped {
						resultType = rightType
					}
					if !leftUntyped || !rightUntyped {
						if !l.constIntegerRepresentable(out, resultType) {
							l.add("hirgen.const.representable", "constant value is not representable by target type "+resultType, expr.Span)
							return nil, "", false
						}
					}
					raw, ok := l.exactIntegerRawForType(out, resultType)
					if !ok {
						return nil, "", false
					}
					return raw, resultType, true
				}
			}
		}
		if leftValue, ok := l.constInt64(leftRaw, leftType); ok {
			if rightValue, ok := l.constInt64(rightRaw, rightType); ok {
				if out, ok := foldInt64Compare(expr.Operator, leftValue, rightValue); ok {
					return out, "Bool", true
				}
				out, ok := foldInt64Binary(expr.Operator, leftValue, rightValue)
				if ok {
					return json.RawMessage(strconv.FormatInt(out, 10)), leftType, true
				}
			}
		}
		leftKind := l.underlyingConstType(leftType)
		rightKind := l.underlyingConstType(rightType)
		untyped := leftUntyped && rightUntyped
		if isComplexType(leftKind) || isComplexType(rightKind) {
			left, leftOK := l.constExactComplex(leftRaw, leftType)
			right, rightOK := l.constExactComplex(rightRaw, rightType)
			if leftOK && rightOK {
				if expr.Operator == "==" || expr.Operator == "!=" {
					equal := constant.CompareRational(left.realPart, right.realPart) == 0 && constant.CompareRational(left.imaginaryPart, right.imaginaryPart) == 0
					if expr.Operator == "!=" {
						equal = !equal
					}
					return boolRaw(equal), "Bool", true
				}
				value, ok := foldExactComplexBinary(expr.Operator, left, right)
				if ok {
					typ := "Complex128"
					if !untyped {
						if !leftUntyped {
							typ = leftType
						} else {
							typ = rightType
						}
					}
					raw, ok := l.complexRawForType(value, typ, untyped)
					if !ok && !untyped {
						l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
					}
					return raw, typ, ok
				}
			}
		}
		if isFloatType(leftKind) || isFloatType(rightKind) {
			left, leftOK := l.constExactRational(leftRaw, leftType)
			right, rightOK := l.constExactRational(rightRaw, rightType)
			if leftOK && rightOK {
				if out, ok := foldExactRationalCompare(expr.Operator, left, right); ok {
					return out, "Bool", true
				}
				value, ok := foldExactRationalBinary(expr.Operator, left, right)
				if ok {
					typ := "Float64"
					if !untyped {
						if !leftUntyped {
							typ = leftType
						} else {
							typ = rightType
						}
					}
					raw, ok := l.rationalRawForType(value, typ, untyped)
					if !ok && !untyped {
						l.add("hirgen.const.representable", "constant value is not representable by target type "+typ, expr.Span)
					}
					return raw, typ, ok
				}
			}
		}
		return nil, "", false
	case ast.ExprConvert:
		raw, typ, ok := l.constValue(derefExpr(expr.Operand), scope)
		if !ok {
			return nil, "", false
		}
		target := l.resolveSourceTypePtr(expr.Type, nil)
		if target == "" {
			return nil, "", false
		}
		return l.convertConstValue(raw, typ, target)
	case ast.ExprCall:
		if target, ok := l.typeConversionCallTarget(expr, scope); ok {
			if len(expr.Args) != 1 {
				return nil, "", false
			}
			raw, typ, ok := l.constValue(expr.Args[0], scope)
			if !ok {
				return nil, "", false
			}
			return l.convertConstValue(raw, typ, target)
		}
		if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent || !l.isBuiltinCallName(expr.Callee.Name, expr.Callee.Span, scope) {
			return nil, "", false
		}
		return l.constBuiltinCallValue(expr, scope)
	default:
		return nil, "", false
	}
}

// Folding removes syntax from the emitted expression but not its dependency identity.
func (l *lowerer) markConstantImports(expr ast.Expression, scope *funcScope) {
	if expr.Kind == ast.ExprIdent {
		if _, local := l.lookupConstValue(expr.Name, scope); !local {
			_, _ = l.dotImportExport(expr.Name, expr.Span)
		}
	}
	if expr.Kind == ast.ExprSelector {
		if export, ok := l.selectorExport(expr, scope); ok {
			l.markImportExport(export.ModulePath, expr.Field)
		}
	}
	for _, child := range []*ast.Expression{expr.Left, expr.Right, expr.Operand, expr.Callee} {
		if child != nil {
			l.markConstantImports(*child, scope)
		}
	}
	for _, child := range expr.Args {
		l.markConstantImports(child, scope)
	}
}

func (l *lowerer) foldUnsignedIntegerUnary(operator string, raw json.RawMessage, typ string) (json.RawMessage, bool) {
	if strings.TrimSpace(operator) != "^" {
		return nil, false
	}
	kind := l.underlyingConstType(typ)
	if !isUnsignedIntegerType(kind) {
		return nil, false
	}
	value, ok := constUint64(raw, kind)
	if !ok {
		return nil, false
	}
	value = normalizeConstUint(^value, kind)
	encoded, err := json.Marshal(strconv.FormatUint(value, 10))
	if err != nil {
		return nil, false
	}
	return json.RawMessage(encoded), true
}

func derefExpr(expr *ast.Expression) ast.Expression {
	if expr == nil {
		return ast.Expression{}
	}
	return *expr
}

func constInt64(raw json.RawMessage, typ string) (int64, bool) {
	if !isIntegerType(typ) {
		return 0, false
	}
	if isUnsignedIntegerType(typ) {
		out, ok := constUint64(raw, typ)
		if !ok || out > uint64(maxInt64Value) {
			return 0, false
		}
		return int64(out), ok
	}
	if value, ok := constExactInteger(raw, typ); ok {
		return constant.SignedDecimalInt64(value)
	}
	if text, ok := rawString(raw); ok {
		out, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, false
		}
		return out, true
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		out, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		return out, true
	}
	return 0, false
}

func constUint64(raw json.RawMessage, typ string) (uint64, bool) {
	if !isIntegerType(typ) {
		return 0, false
	}
	if isSignedIntegerType(typ) {
		if out, ok := constInt64(raw, typ); ok {
			if out < 0 {
				return 0, false
			}
			return uint64(out), true
		}
	}
	if value, ok := constExactInteger(raw, typ); ok && !strings.HasPrefix(value, "-") {
		out, err := strconv.ParseUint(value, 10, 64)
		return out, err == nil
	}
	if text, ok := rawString(raw); ok {
		out, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return 0, false
		}
		return out, true
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		out, err := strconv.ParseUint(number.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		return out, true
	}
	return 0, false
}

func rawString(raw json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

func integerRawText(raw json.RawMessage) (string, bool) {
	if text, ok := rawString(raw); ok {
		return strings.TrimSpace(text), true
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return strings.TrimSpace(number.String()), true
	}
	return "", false
}

func constExactInteger(raw json.RawMessage, typ string) (string, bool) {
	if !isIntegerType(typ) {
		return "", false
	}
	text, ok := integerRawText(raw)
	if !ok || text == "" || strings.ContainsAny(text, ".eEiI") {
		return "", false
	}
	value, ok := constant.NormalizeSignedDecimal(text)
	if !ok {
		return "", false
	}
	if isUnsignedIntegerType(typ) && strings.HasPrefix(value, "-") {
		return "", false
	}
	return value, true
}

func (l *lowerer) constExactInteger(raw json.RawMessage, typ string) (string, bool) {
	if value, ok := constExactInteger(raw, typ); ok {
		return value, true
	}
	underlying := l.underlyingConstType(typ)
	if underlying == typ {
		return "", false
	}
	return constExactInteger(raw, underlying)
}

func (l *lowerer) constInt64(raw json.RawMessage, typ string) (int64, bool) {
	if out, ok := constInt64(raw, typ); ok {
		return out, true
	}
	underlying := l.underlyingConstType(typ)
	if underlying == typ {
		return 0, false
	}
	return constInt64(raw, underlying)
}
