package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func (l *lowerer) lowerNewBuiltin(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	if isNilLiteral(expr.Args[0]) {
		l.add("hirgen.builtin.new.nil", "new builtin cannot allocate untyped nil", expr.Args[0].Span)
		return ir.Expression{}, false
	}
	if isTypeArgumentExpression(expr.Args[0]) {
		typ := l.resolveSourceTypePtr(expr.Args[0].Type, nil)
		if typ == "Void" {
			l.add("hirgen.builtin.new.type", "new builtin requires a concrete type or typed expression", expr.Args[0].Span)
			return ir.Expression{}, false
		}
		local := l.newSyntheticLocal(scope, "new.object", typ)
		zero := ir.Expression{Kind: ir.ExprZero, Type: l.hirType(typ)}
		addr := ir.Expression{Kind: ir.ExprAddressOf, Local: local}
		return ir.Expression{Kind: ir.ExprLet, Local: local, Bind: &zero, Body: &addr}, true
	}
	if typ, ok := l.resolveNamedTypeArgument(expr.Args[0], scope); ok {
		local := l.newSyntheticLocal(scope, "new.object", typ)
		zero := ir.Expression{Kind: ir.ExprZero, Type: l.hirType(typ)}
		addr := ir.Expression{Kind: ir.ExprAddressOf, Local: local}
		return ir.Expression{Kind: ir.ExprLet, Local: local, Bind: &zero, Body: &addr}, true
	}
	targetType := l.defaultedExpressionType(expr.Args[0], scope)
	if strings.TrimSpace(targetType) == "" || targetType == "Void" {
		l.add("hirgen.builtin.new.type", "new builtin requires a concrete type or typed expression", expr.Args[0].Span)
		return ir.Expression{}, false
	}
	value, ok := l.lowerExpressionInType(expr.Args[0], targetType, scope)
	if !ok {
		return ir.Expression{}, false
	}
	local := l.newSyntheticLocal(scope, "new.object", targetType)
	addr := ir.Expression{Kind: ir.ExprAddressOf, Local: local}
	return ir.Expression{Kind: ir.ExprLet, Local: local, Bind: &value, Body: &addr}, true
}

func isTypeArgumentExpression(expr ast.Expression) bool {
	return expr.Kind == ast.ExprIdent && strings.TrimSpace(expr.Name) == "type" && expr.Type != nil && expr.Type.Kind != ast.TypeInvalid
}
