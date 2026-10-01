package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) isBuiltinCallName(name string, span source.Span, scope *funcScope) bool {
	if !check.IsPredeclaredBuiltin(strings.TrimSpace(name)) {
		return false
	}
	return !l.isNameBound(name, span, scope)
}

func (l *lowerer) typeConversionCallTarget(expr ast.Expression, scope *funcScope) (string, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
		return "", false
	}
	if expr.Callee.Name == "type" && expr.Callee.Type != nil && expr.Callee.Type.Kind != ast.TypeInvalid {
		target := l.resolveSourceType(*expr.Callee.Type)
		return target, target != ""
	}
	return l.resolveNamedTypeArgument(*expr.Callee, scope)
}

func (l *lowerer) resolveNamedTypeArgument(expr ast.Expression, scope *funcScope) (string, bool) {
	switch expr.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if name == "" || l.isValueNameBound(name, expr.Span, scope) {
			return "", false
		}
		if _, ok := l.typeDecls[name]; ok {
			return l.resolveType(name), true
		}
		if _, ok := l.typeAliases[name]; ok {
			return l.resolveType(name), true
		}
		if export, ok := l.dotImportExport(name, expr.Span); ok && export.Kind == check.ObjectType {
			return l.importedTypeCanonicalName(name, export), true
		}
		return sourcePredeclaredType(name)
	case ast.ExprSelector:
		if _, ok := l.selectorTypeExport(expr, scope); ok {
			if typ := l.selectorConversionType(expr, scope); typ != "" {
				return typ, true
			}
		}
	}
	return "", false
}
