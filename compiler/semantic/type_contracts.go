package semantic

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

// validateTypeContracts runs after declaration identities and underlying types
// are complete, so forward references use the same rules as resolved types.
func (a *analyzer) validateTypeContracts(program *ast.Program) {
	inferredArrays := make(map[ast.NodeID]bool)
	ast.WalkExpressions(program, func(expr *ast.Expression) {
		if expr.Type == nil {
			return
		}
		if expr.Kind == ast.ExprComposite && expr.Type.Kind == ast.TypeArray && expr.Type.LenInfer {
			inferredArrays[expr.Type.NodeID] = true
		}
		a.validateValueType(a.resolvedType(*expr.Type), expr.Type.Span)
	})
	ast.WalkTypes(program, func(typ *ast.TypeExpr) {
		if typ.Kind == ast.TypeArray && typ.LenInfer && !inferredArrays[typ.NodeID] {
			a.addDiagnostic("semantic.type.array.infer", "array length ellipsis is only valid in an array composite literal type", typ.Span)
		}
		if typ.Kind == ast.TypeMap && typ.Key != nil {
			key := a.resolvedType(*typ.Key)
			if a.info.TypeExact(key) && !a.info.Relations.MapKeyAllowed(key).OK {
				a.addDiagnostic("semantic.map.key.comparable", "map key type must be comparable", typ.Key.Span)
			}
		}
		if typ.Kind == ast.TypeInterface {
			a.validateTypeSetConstraint(*typ)
			a.validateInterfaceMethods(*typ)
			return
		}
		// Container elements, fields and function signatures always denote
		// value types, even inside a named type or a generic constraint.
		if typ.Elem != nil {
			a.validateValueType(a.resolvedType(*typ.Elem), typ.Elem.Span)
		}
		if typ.Key != nil {
			a.validateValueType(a.resolvedType(*typ.Key), typ.Key.Span)
		}
		for _, fields := range [][]ast.Field{typ.Fields, typ.Params, typ.Results} {
			for _, field := range fields {
				a.validateValueType(a.resolvedType(field.Type), field.Span)
			}
		}
	})
}

func (a *analyzer) validateValueType(ref types.TypeRef, span source.Span) {
	if node, ok := a.info.TypeTable.IsInterface(ref); ok && node.TypeSet {
		a.addDiagnostic("semantic.general_interface.value", "general interface cannot be used as value type", span)
	}
}

func (a *analyzer) validateInterfaceMethods(typ ast.TypeExpr) {
	node, ok := a.info.TypeTable.IsInterface(a.resolvedType(typ))
	if !ok {
		return
	}
	type methodKey struct{ module, name string }
	methods := make(map[methodKey]types.FunctionSignature)
	for _, method := range node.Methods {
		key := methodKey{name: method.Name}
		if !isExported(method.Name) {
			key.module = method.ModulePath
		}
		if previous, exists := methods[key]; exists && !a.info.Relations.SignatureIdentical(previous, method.Signature).OK {
			a.addDiagnostic("semantic.interface.method.conflict", "embedded interface methods have conflicting signatures", typ.Span)
		} else {
			methods[key] = method.Signature
		}
	}
}
