package ast

import "github.com/d7z-team/mini-go/compiler/source"

// RewriteDeclSourcePaths rewrites every source position owned by a declaration.
func RewriteDeclSourcePaths(decl *Decl, rewrite func(string) string) {
	if decl == nil || rewrite == nil {
		return
	}
	rewriteSpan(&decl.Span, rewrite)
	if !decl.hasPayload() {
		return
	}
	switch decl.Kind {
	case DeclImport:
		rewriteSpan(&decl.Import.PathSpan, rewrite)
		rewriteIdentifier(&decl.Import.AliasID, rewrite)
	case DeclConst:
		rewriteValueDeclPaths(decl.Const, rewrite)
	case DeclVar:
		rewriteValueDeclPaths(decl.Var, rewrite)
	case DeclType:
		rewriteIdentifier(&decl.Type.NameID, rewrite)
		for i := range decl.Type.TypeParams {
			rewriteTypeParamPaths(&decl.Type.TypeParams[i], rewrite)
		}
		rewriteTypePaths(&decl.Type.Type, rewrite)
	case DeclFunc:
		rewriteFuncPaths(decl.Func, rewrite)
	}
}

func rewriteValueDeclPaths(decl *ValueDecl, rewrite func(string) string) {
	for i := range decl.NameIDs {
		rewriteIdentifier(&decl.NameIDs[i], rewrite)
	}
	rewriteTypePaths(&decl.Type, rewrite)
	for i := range decl.Values {
		rewriteExpressionPaths(&decl.Values[i], rewrite)
	}
}

func rewriteFuncPaths(decl *FuncDecl, rewrite func(string) string) {
	rewriteIdentifier(&decl.NameID, rewrite)
	if decl.Receiver != nil {
		rewriteFieldPaths(decl.Receiver, rewrite)
	}
	for i := range decl.TypeParams {
		rewriteTypeParamPaths(&decl.TypeParams[i], rewrite)
	}
	for i := range decl.Params {
		rewriteFieldPaths(&decl.Params[i], rewrite)
	}
	for i := range decl.Results {
		rewriteFieldPaths(&decl.Results[i], rewrite)
	}
	rewriteBlockPaths(&decl.Body, rewrite)
}

func rewriteTypeParamPaths(param *TypeParam, rewrite func(string) string) {
	rewriteSpan(&param.Span, rewrite)
	rewriteIdentifier(&param.NameID, rewrite)
	rewriteTypePaths(&param.Constraint, rewrite)
}

func rewriteFieldPaths(field *Field, rewrite func(string) string) {
	rewriteSpan(&field.Span, rewrite)
	rewriteIdentifier(&field.NameID, rewrite)
	rewriteTypePaths(&field.Type, rewrite)
}

func rewriteTypePaths(typ *TypeExpr, rewrite func(string) string) {
	if typ == nil || typ.Kind == TypeInvalid {
		return
	}
	rewriteSpan(&typ.Span, rewrite)
	rewriteIdentifier(&typ.NameID, rewrite)
	rewriteIdentifier(&typ.QualifierID, rewrite)
	rewriteTypePaths(typ.Base, rewrite)
	for i := range typ.TypeArgs {
		rewriteTypePaths(&typ.TypeArgs[i], rewrite)
	}
	rewriteTypePaths(typ.Elem, rewrite)
	rewriteTypePaths(typ.Key, rewrite)
	rewriteExpressionPaths(typ.Len, rewrite)
	for i := range typ.Params {
		rewriteFieldPaths(&typ.Params[i], rewrite)
	}
	for i := range typ.Results {
		rewriteFieldPaths(&typ.Results[i], rewrite)
	}
	for i := range typ.Fields {
		rewriteFieldPaths(&typ.Fields[i], rewrite)
	}
	for i := range typ.Methods {
		rewriteFuncPaths(&typ.Methods[i], rewrite)
	}
	for i := range typ.Embeds {
		rewriteTypePaths(&typ.Embeds[i], rewrite)
	}
	for i := range typ.Terms {
		rewriteSpan(&typ.Terms[i].Span, rewrite)
		rewriteTypePaths(&typ.Terms[i].Type, rewrite)
	}
}

func rewriteBlockPaths(block *BlockStmt, rewrite func(string) string) {
	if block == nil {
		return
	}
	rewriteSpan(&block.Span, rewrite)
	for i := range block.Stmts {
		rewriteStatementPaths(&block.Stmts[i], rewrite)
	}
}

func rewriteStatementPaths(stmt *Statement, rewrite func(string) string) {
	if stmt == nil {
		return
	}
	rewriteSpan(&stmt.Span, rewrite)
	rewriteIdentifier(&stmt.LabelID, rewrite)
	rewriteIdentifier(&stmt.TypeSwitchID, rewrite)
	for i := range stmt.Decls {
		RewriteDeclSourcePaths(&stmt.Decls[i], rewrite)
	}
	rewriteExpressionPaths(stmt.Expr, rewrite)
	for i := range stmt.Left {
		rewriteExpressionPaths(&stmt.Left[i], rewrite)
	}
	for i := range stmt.Right {
		rewriteExpressionPaths(&stmt.Right[i], rewrite)
	}
	rewriteBlockPaths(&stmt.Body, rewrite)
	rewriteStatementPaths(stmt.Init, rewrite)
	rewriteExpressionPaths(stmt.Cond, rewrite)
	rewriteStatementPaths(stmt.Post, rewrite)
	rewriteStatementPaths(stmt.Else, rewrite)
	rewriteExpressionPaths(stmt.Key, rewrite)
	rewriteExpressionPaths(stmt.Value, rewrite)
	rewriteExpressionPaths(stmt.Range, rewrite)
	for i := range stmt.Cases {
		clause := &stmt.Cases[i]
		rewriteSpan(&clause.Span, rewrite)
		for j := range clause.Values {
			rewriteExpressionPaths(&clause.Values[j], rewrite)
		}
		for j := range clause.Types {
			rewriteTypePaths(&clause.Types[j], rewrite)
		}
		rewriteStatementPaths(clause.Comm, rewrite)
		rewriteBlockPaths(&clause.Body, rewrite)
	}
	for i := range stmt.Results {
		rewriteExpressionPaths(&stmt.Results[i], rewrite)
	}
}

func rewriteExpressionPaths(expr *Expression, rewrite func(string) string) {
	if expr == nil || expr.Kind == ExprInvalid {
		return
	}
	rewriteSpan(&expr.Span, rewrite)
	rewriteIdentifier(&expr.NameID, rewrite)
	rewriteTypePaths(expr.Type, rewrite)
	rewriteExpressionPaths(expr.Left, rewrite)
	rewriteExpressionPaths(expr.Right, rewrite)
	rewriteExpressionPaths(expr.Operand, rewrite)
	rewriteExpressionPaths(expr.Callee, rewrite)
	for i := range expr.Args {
		rewriteExpressionPaths(&expr.Args[i], rewrite)
	}
	rewriteExpressionPaths(expr.Index, rewrite)
	rewriteExpressionPaths(expr.Start, rewrite)
	rewriteExpressionPaths(expr.End, rewrite)
	rewriteExpressionPaths(expr.Max, rewrite)
	for i := range expr.Items {
		rewriteExpressionPaths(expr.Items[i].Key, rewrite)
		rewriteExpressionPaths(&expr.Items[i].Value, rewrite)
	}
	if expr.Kind == ExprFunc && expr.Func != nil {
		rewriteFuncPaths(expr.Func, rewrite)
	}
}

func rewriteIdentifier(identifier *Identifier, rewrite func(string) string) {
	if identifier != nil {
		rewriteSpan(&identifier.Span, rewrite)
	}
}

func rewriteSpan(span *source.Span, rewrite func(string) string) {
	if span == nil {
		return
	}
	if span.Start.File != "" {
		span.Start.File = rewrite(span.Start.File)
	}
	if span.End.File != "" {
		span.End.File = rewrite(span.End.File)
	}
}
