package lower

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
)

type localTypeEnvironment map[string]string

func (l *lowerer) rewriteLocalTypes(program *ast.Program) {
	if program == nil {
		return
	}
	for fileIndex := range program.Files {
		file := &program.Files[fileIndex]
		for declIndex := range file.Decls {
			decl := &file.Decls[declIndex]
			if decl.Kind != ast.DeclFunc {
				continue
			}
			l.rewriteLocalTypeFunc(decl.Func, localTypeEnvironment{})
		}
	}
}

func (l *lowerer) rewriteLocalTypeFunc(fn *ast.FuncDecl, inherited localTypeEnvironment) {
	if fn == nil {
		return
	}
	env := cloneLocalTypeEnvironment(inherited)
	for i := range fn.Params {
		l.rewriteLocalTypeExpr(&fn.Params[i].Type, env)
	}
	for i := range fn.Results {
		l.rewriteLocalTypeExpr(&fn.Results[i].Type, env)
	}
	l.rewriteLocalTypeBlock(&fn.Body, env)
}

func cloneLocalTypeEnvironment(env localTypeEnvironment) localTypeEnvironment {
	out := make(localTypeEnvironment, len(env))
	for name, typ := range env {
		out[name] = typ
	}
	return out
}

func statementDeclPtrs(stmt *ast.Statement) []*ast.Decl {
	if stmt == nil {
		return nil
	}
	out := make([]*ast.Decl, 0, len(stmt.Decls))
	for i := range stmt.Decls {
		out = append(out, &stmt.Decls[i])
	}
	return out
}

func (l *lowerer) rewriteLocalTypeBlock(block *ast.BlockStmt, inherited localTypeEnvironment) {
	if block == nil {
		return
	}
	env := cloneLocalTypeEnvironment(inherited)
	for i := range block.Stmts {
		l.rewriteLocalTypeStatement(&block.Stmts[i], env)
	}
}

func (l *lowerer) rewriteLocalTypeStatement(stmt *ast.Statement, env localTypeEnvironment) {
	if stmt == nil {
		return
	}
	for _, decl := range statementDeclPtrs(stmt) {
		switch decl.Kind {
		case ast.DeclType:
			name := strings.TrimSpace(decl.Type.Name)
			if name == "" || isBlankIdentifier(name) {
				break
			}
			if decl.Type.Alias {
				l.rewriteLocalTypeExpr(&decl.Type.Type, env)
				env[name] = l.resolveSourceType(decl.Type.Type)
				break
			}
			synthetic := localTypeName(decl.NodeID, name)
			env[name] = synthetic
			l.rewriteLocalTypeExpr(&decl.Type.Type, env)
			l.typeDecls[synthetic] = decl.Type.Type
			decl.Type.Name = synthetic
		case ast.DeclVar:
			l.rewriteLocalTypeExpr(&decl.Var.Type, env)
			for i := range decl.Var.Values {
				l.rewriteLocalTypeExpression(&decl.Var.Values[i], env)
			}
		case ast.DeclConst:
			l.rewriteLocalTypeExpr(&decl.Const.Type, env)
			for i := range decl.Const.Values {
				l.rewriteLocalTypeExpression(&decl.Const.Values[i], env)
			}
		}
	}
	if stmt.Expr != nil {
		l.rewriteLocalTypeExpression(stmt.Expr, env)
	}
	for i := range stmt.Left {
		l.rewriteLocalTypeExpression(&stmt.Left[i], env)
	}
	for i := range stmt.Right {
		l.rewriteLocalTypeExpression(&stmt.Right[i], env)
	}
	for i := range stmt.Results {
		l.rewriteLocalTypeExpression(&stmt.Results[i], env)
	}
	if stmt.Init != nil {
		l.rewriteLocalTypeStatement(stmt.Init, cloneLocalTypeEnvironment(env))
	}
	if stmt.Cond != nil {
		l.rewriteLocalTypeExpression(stmt.Cond, env)
	}
	if stmt.Post != nil {
		l.rewriteLocalTypeStatement(stmt.Post, cloneLocalTypeEnvironment(env))
	}
	if stmt.Else != nil {
		l.rewriteLocalTypeStatement(stmt.Else, cloneLocalTypeEnvironment(env))
	}
	if stmt.Key != nil {
		l.rewriteLocalTypeExpression(stmt.Key, env)
	}
	if stmt.Value != nil {
		l.rewriteLocalTypeExpression(stmt.Value, env)
	}
	if stmt.Range != nil {
		l.rewriteLocalTypeExpression(stmt.Range, env)
	}
	switch stmt.Kind {
	case ast.StmtBlock, ast.StmtIf, ast.StmtFor, ast.StmtRange:
		l.rewriteLocalTypeBlock(&stmt.Body, env)
	case ast.StmtSwitch, ast.StmtSelect:
		for i := range stmt.Cases {
			clause := &stmt.Cases[i]
			for j := range clause.Values {
				l.rewriteLocalTypeExpression(&clause.Values[j], env)
			}
			for j := range clause.Types {
				l.rewriteLocalTypeExpr(&clause.Types[j], env)
			}
			if clause.Comm != nil {
				l.rewriteLocalTypeStatement(clause.Comm, cloneLocalTypeEnvironment(env))
			}
			l.rewriteLocalTypeBlock(&clause.Body, env)
		}
	}
}

func (l *lowerer) rewriteLocalTypeExpression(expr *ast.Expression, env localTypeEnvironment) {
	if expr == nil {
		return
	}
	l.rewriteLocalTypeExpr(expr.Type, env)
	if expr.Kind == ast.ExprIdent {
		typ := env[strings.TrimSpace(expr.Name)]
		if typ != "" {
			expr.Name = typ
		}
	}
	if expr.Kind == ast.ExprCall && expr.Callee != nil && expr.Callee.Kind == ast.ExprIdent {
		typ := env[strings.TrimSpace(expr.Callee.Name)]
		if typ != "" {
			expr.Callee.Name = typ
		}
	}
	if expr.Left != nil {
		l.rewriteLocalTypeExpression(expr.Left, env)
	}
	if expr.Right != nil {
		l.rewriteLocalTypeExpression(expr.Right, env)
	}
	if expr.Operand != nil {
		l.rewriteLocalTypeExpression(expr.Operand, env)
	}
	if expr.Callee != nil {
		l.rewriteLocalTypeExpression(expr.Callee, env)
	}
	if expr.Index != nil {
		l.rewriteLocalTypeExpression(expr.Index, env)
	}
	if expr.Start != nil {
		l.rewriteLocalTypeExpression(expr.Start, env)
	}
	if expr.End != nil {
		l.rewriteLocalTypeExpression(expr.End, env)
	}
	if expr.Max != nil {
		l.rewriteLocalTypeExpression(expr.Max, env)
	}
	for i := range expr.Args {
		l.rewriteLocalTypeExpression(&expr.Args[i], env)
	}
	for i := range expr.Items {
		if expr.Items[i].Key != nil {
			l.rewriteLocalTypeExpression(expr.Items[i].Key, env)
		}
		l.rewriteLocalTypeExpression(&expr.Items[i].Value, env)
	}
	if expr.Kind == ast.ExprFunc {
		l.rewriteLocalTypeFunc(expr.Func, env)
	}
}

func (l *lowerer) rewriteLocalTypeExpr(typ *ast.TypeExpr, env localTypeEnvironment) {
	if typ == nil {
		return
	}
	if typ.Kind == ast.TypeName {
		name := strings.TrimSpace(typ.Name)
		target := env[name]
		if target != "" {
			typ.Name = target
		}
	}
	if typ.Elem != nil {
		l.rewriteLocalTypeExpr(typ.Elem, env)
	}
	if typ.Key != nil {
		l.rewriteLocalTypeExpr(typ.Key, env)
	}
	if typ.Len != nil {
		l.rewriteLocalTypeExpression(typ.Len, env)
	}
	for i := range typ.Params {
		l.rewriteLocalTypeExpr(&typ.Params[i].Type, env)
	}
	for i := range typ.Results {
		l.rewriteLocalTypeExpr(&typ.Results[i].Type, env)
	}
	for i := range typ.Fields {
		l.rewriteLocalTypeExpr(&typ.Fields[i].Type, env)
	}
	for i := range typ.Embeds {
		l.rewriteLocalTypeExpr(&typ.Embeds[i], env)
	}
	for i := range typ.Terms {
		l.rewriteLocalTypeExpr(&typ.Terms[i].Type, env)
	}
	for i := range typ.Methods {
		l.rewriteLocalTypeFunc(&typ.Methods[i], env)
	}
}

func localTypeName(node ast.NodeID, name string) string {
	return "local_type_" + strconv.FormatUint(uint64(node), 10) + "_" + strings.TrimSpace(name)
}

func (l *lowerer) registerSymbol(seen map[string]source.Span, name string, span source.Span) bool {
	if name == "" {
		return false
	}
	if _, exists := seen[name]; exists {
		l.add("hirgen.symbol.duplicate", "duplicate top-level symbol", span)
		return false
	}
	seen[name] = span
	return true
}

func (l *lowerer) registerPackageIdentifier(seen map[string]source.Span, name string, span source.Span) bool {
	if name == "init" {
		l.add("hirgen.init.declaration", "init identifier must be declared as a function", span)
		return false
	}
	return l.registerSymbol(seen, name, span)
}
