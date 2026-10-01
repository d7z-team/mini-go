package semantic

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func (a *analyzer) validateResolvedIdentifiers(program *ast.Program) {
	ast.WalkExpressions(program, func(expr *ast.Expression) {
		if expr.Kind != ast.ExprIdent {
			return
		}
		if expr.Name == "_" || expr.Name == "type" && expr.Type != nil && expr.Type.Kind != ast.TypeInvalid {
			return
		}
		// Struct field keys and declaration names are not value expressions.
		if _, analyzed := a.info.NodeScopes[expr.NodeID]; !analyzed {
			return
		}
		if _, resolved := a.info.Uses[expr.NodeID]; resolved {
			return
		}
		if _, defined := a.info.Defs[expr.NodeID]; defined {
			return
		}
		a.addDiagnostic("semantic.identifier.unknown", "unknown identifier "+strings.TrimSpace(expr.Name), expr.Span)
	})
}
