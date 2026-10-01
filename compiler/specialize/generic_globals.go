package specialize

import (
	"sort"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

// Accessors preserve definition-package state when a template is instantiated
// elsewhere. They use ordinary calls and addresses and are not source exports.
func (s *genericSpecializer) appendTemplateGlobalAccessors(output *ast.Program, original ast.Program) {
	if len(output.Files) == 0 {
		return
	}
	globals := make(map[string]check.Object)
	for _, file := range original.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclFunc || len(decl.Func.TypeParams) == 0 && (decl.Func.Receiver == nil || s.types[genericReceiverName(decl.Func.Receiver.Type)].decl.Kind == "") {
				continue
			}
			template := ast.Program{Files: []ast.File{{Decls: []ast.Decl{decl}}}}
			ast.WalkExpressions(&template, func(expr *ast.Expression) {
				object, ok := s.info.Object(s.info.Uses[expr.NodeID])
				if ok && object.Scope == s.info.PackageScope && object.Kind == check.ObjectVar && !object.Exported {
					globals[object.Name] = object
				}
			})
		}
	}
	names := make([]string, 0, len(globals))
	for name := range globals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		object := globals[name]
		span := object.Definition.Span
		typ := s.sourceTypeExpr(object.Type, span)
		pointer := ast.TypeExpr{Kind: ast.TypePointer, Elem: &typ, Span: span}
		value := ast.Expression{Kind: ast.ExprIdent, Name: name, Span: span}
		address := ast.Expression{Kind: ast.ExprAddr, Operand: &value, Span: span}
		decl := ast.Decl{Kind: ast.DeclFunc, Span: span, Func: &ast.FuncDecl{Name: "_generic_global_address_" + name, Results: []ast.Field{{Type: pointer}}, Body: ast.BlockStmt{Stmts: []ast.Statement{{Kind: ast.StmtReturn, Results: []ast.Expression{address}, Span: span}}}}}
		output.Files[0].Decls = append(output.Files[0].Decls, decl)
	}
}
