package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) lowerMinMaxCall(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	if expr.Ellipsis {
		l.add("hirgen.builtin.minmax.ellipsis", "min and max do not permit slice ellipsis", expr.Span)
		return ir.Expression{}, false
	}
	targetType, ok := l.minMaxTargetType(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	args := make([]ir.Expression, 0, len(expr.Args))
	for _, arg := range expr.Args {
		lowered, ok := l.lowerExpressionInType(arg, targetType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		args = append(args, lowered)
	}
	return ir.Expression{
		Kind:        ir.ExprMinMax,
		Operator:    strings.TrimSpace(expr.Callee.Name),
		Args:        args,
		Type:        l.hirType(targetType),
		ResultCount: 1,
	}, true
}

func (l *lowerer) minMaxTargetType(expr ast.Expression, scope *funcScope) (string, bool) {
	if len(expr.Args) == 0 {
		l.add("hirgen.builtin.minmax.arg_count", "min and max require at least one argument", expr.Span)
		return "", false
	}
	target := ""
	untypedTypes := make([]string, 0, len(expr.Args))
	for _, arg := range expr.Args {
		typ := strings.TrimSpace(l.expressionType(arg, scope))
		if typ == "" || typ == "Any" {
			l.add("hirgen.builtin.minmax.type", "min and max arguments require ordered values", arg.Span)
			return "", false
		}
		if l.untypedConstExpression(arg, scope) {
			untypedTypes = append(untypedTypes, typ)
			continue
		}
		if target == "" {
			target = typ
			continue
		}
		if typ != target {
			l.add("hirgen.builtin.minmax.types", "min and max arguments must have identical types", arg.Span)
			return "", false
		}
	}
	if target == "" {
		inferred, ok := commonMinMaxUntypedType(untypedTypes)
		if !ok {
			l.add("hirgen.builtin.minmax.types", "min and max untyped arguments must have a common ordered type", expr.Span)
			return "", false
		}
		target = inferred
	}
	underlying := l.resolveNamedUnderlyingType(target)
	for _, typ := range untypedTypes {
		if !minMaxUntypedAssignable(typ, underlying) {
			l.add("hirgen.builtin.minmax.types", "min and max arguments must have compatible ordered types", expr.Span)
			return "", false
		}
	}
	if !l.validateMinMaxType(target, expr.Span) {
		return "", false
	}
	return target, true
}

func commonMinMaxUntypedType(types []string) (string, bool) {
	if len(types) == 0 {
		return "", false
	}
	hasFloat := false
	for _, typ := range types {
		typ = strings.TrimSpace(typ)
		switch {
		case typ == "String":
			if hasFloat {
				return "", false
			}
		case isFloatType(typ):
			hasFloat = true
		case isIntegerType(typ):
		default:
			return "", false
		}
	}
	if hasFloat {
		for _, typ := range types {
			if strings.TrimSpace(typ) == "String" {
				return "", false
			}
		}
		return "Float64", true
	}
	for _, typ := range types {
		if strings.TrimSpace(typ) == "String" {
			return "String", true
		}
	}
	return "Int", true
}

func minMaxUntypedAssignable(source, target string) bool {
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	if target == "String" {
		return source == "String"
	}
	if isFloatType(target) {
		return isIntegerType(source) || isFloatType(source)
	}
	if isIntegerType(target) {
		return isIntegerType(source)
	}
	return false
}

func (l *lowerer) validateMinMaxType(typ string, span source.Span) bool {
	underlying := l.resolveNamedUnderlyingType(strings.TrimSpace(typ))
	if underlying == "String" || isIntegerType(underlying) || isFloatType(underlying) {
		return true
	}
	l.add("hirgen.builtin.minmax.type", "min and max require integer, floating-point, or string arguments", span)
	return false
}
