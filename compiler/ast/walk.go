package ast

import "github.com/d7z-team/mini-go/compiler/source"

// WalkExpressions visits every source expression once in lexical AST order.
func WalkExpressions(program *Program, visit func(*Expression)) {
	walkNodes(program, visit, nil, nil, nil)
}

// WalkTypes visits every source type expression once in lexical AST order.
func WalkTypes(program *Program, visit func(*TypeExpr)) {
	walkNodes(program, nil, visit, nil, nil)
}

// WalkSpans visits the source ranges of declarations, blocks, statements,
// expressions and types in lexical AST order.
func WalkSpans(program *Program, visit func(source.Span)) {
	walkNodes(program, nil, nil, visit, nil)
}

// EstimatedDeclBytes conservatively accounts for a declaration's owned tree,
// including source names and literal contents. Shared strings may be counted
// more than once. An overflowing estimate saturates at the largest int64.
func EstimatedDeclBytes(decl Decl) int64 {
	const maximum = int64(1<<63 - 1)
	var size int64
	measure := func(base int64, texts ...string) {
		if base > maximum-size {
			size = maximum
			return
		}
		size += base
		for _, text := range texts {
			if int64(len(text)) > maximum-size {
				size = maximum
				return
			}
			size += int64(len(text))
		}
	}
	program := Program{Files: []File{{Decls: []Decl{decl}}}}
	walkNodes(&program, nil, nil, func(span source.Span) {
		measure(64, span.Start.File, span.End.File)
	}, measure)
	return size
}

func walkNodes(program *Program, visitExpr func(*Expression), visitType func(*TypeExpr), visitSpan func(source.Span), measure func(int64, ...string)) {
	if program == nil || visitExpr == nil && visitType == nil && visitSpan == nil && measure == nil {
		return
	}
	var walkExpr func(*Expression)
	var walkType func(*TypeExpr)
	var walkDecl func(*Decl)
	var walkStmt func(*Statement)
	var walkBlock func(*BlockStmt)
	var walkFunc func(*FuncDecl)
	walkExpr = func(expr *Expression) {
		if expr == nil {
			return
		}
		if visitExpr != nil {
			visitExpr(expr)
		}
		if measure != nil {
			measure(2048, string(expr.Kind), expr.Name, expr.NameID.Text, expr.Literal, expr.Operator, expr.Field)
			measure(0, expr.NameID.Span.Start.File, expr.NameID.Span.End.File)
			for _, file := range expr.EmbedFiles {
				measure(int64(len(file.Data))+32, file.Path)
			}
		}
		if visitSpan != nil {
			visitSpan(expr.Span)
		}
		walkType(expr.Type)
		walkExpr(expr.Left)
		walkExpr(expr.Right)
		walkExpr(expr.Operand)
		walkExpr(expr.Callee)
		for i := range expr.Args {
			walkExpr(&expr.Args[i])
		}
		walkExpr(expr.Index)
		walkExpr(expr.Start)
		walkExpr(expr.End)
		walkExpr(expr.Max)
		for i := range expr.Items {
			walkExpr(expr.Items[i].Key)
			walkExpr(&expr.Items[i].Value)
		}
		if expr.Kind == ExprFunc || measure != nil {
			walkFunc(expr.Func)
		}
	}
	walkType = func(typ *TypeExpr) {
		if typ == nil {
			return
		}
		if visitType != nil {
			visitType(typ)
		}
		if measure != nil {
			measure(512, string(typ.Kind), typ.Name, typ.NameID.Text, typ.QualifierID.Text, typ.Direction)
			measure(0, typ.NameID.Span.Start.File, typ.NameID.Span.End.File, typ.QualifierID.Span.Start.File, typ.QualifierID.Span.End.File)
		}
		if visitSpan != nil {
			visitSpan(typ.Span)
		}
		walkType(typ.Base)
		walkType(typ.Elem)
		walkType(typ.Key)
		walkExpr(typ.Len)
		for _, fields := range [][]Field{typ.Params, typ.Results, typ.Fields} {
			for i := range fields {
				if measure != nil {
					measure(128, fields[i].Name, fields[i].NameID.Text, fields[i].Tag, fields[i].Span.Start.File, fields[i].Span.End.File)
					measure(0, fields[i].NameID.Span.Start.File, fields[i].NameID.Span.End.File)
				}
				walkType(&fields[i].Type)
			}
		}
		for i := range typ.Methods {
			walkFunc(&typ.Methods[i])
		}
		for i := range typ.Embeds {
			walkType(&typ.Embeds[i])
		}
		for i := range typ.Terms {
			if measure != nil {
				measure(80, typ.Terms[i].Span.Start.File, typ.Terms[i].Span.End.File)
			}
			walkType(&typ.Terms[i].Type)
		}
		for i := range typ.TypeArgs {
			walkType(&typ.TypeArgs[i])
		}
	}
	walkBlock = func(block *BlockStmt) {
		if block == nil {
			return
		}
		if visitSpan != nil {
			visitSpan(block.Span)
		}
		if measure != nil {
			measure(128)
		}
		for i := range block.Stmts {
			walkStmt(&block.Stmts[i])
		}
	}
	walkFunc = func(function *FuncDecl) {
		if function == nil {
			return
		}
		if measure != nil {
			measure(256, function.Name, function.NameID.Text, function.NameID.Span.Start.File, function.NameID.Span.End.File)
		}
		if function.Receiver != nil {
			if measure != nil {
				measure(128, function.Receiver.Name, function.Receiver.NameID.Text, function.Receiver.Tag)
				measure(0, function.Receiver.Span.Start.File, function.Receiver.Span.End.File, function.Receiver.NameID.Span.Start.File, function.Receiver.NameID.Span.End.File)
			}
			walkType(&function.Receiver.Type)
		}
		for i := range function.TypeParams {
			if measure != nil {
				measure(128, function.TypeParams[i].Name, function.TypeParams[i].NameID.Text)
				measure(0, function.TypeParams[i].Span.Start.File, function.TypeParams[i].Span.End.File, function.TypeParams[i].NameID.Span.Start.File, function.TypeParams[i].NameID.Span.End.File)
			}
			walkType(&function.TypeParams[i].Constraint)
		}
		for _, fields := range [][]Field{function.Params, function.Results} {
			for i := range fields {
				if measure != nil {
					measure(128, fields[i].Name, fields[i].NameID.Text, fields[i].Tag, fields[i].Span.Start.File, fields[i].Span.End.File)
					measure(0, fields[i].NameID.Span.Start.File, fields[i].NameID.Span.End.File)
				}
				walkType(&fields[i].Type)
			}
		}
		walkBlock(&function.Body)
	}
	walkDecl = func(decl *Decl) {
		if decl == nil {
			return
		}
		if visitSpan != nil {
			visitSpan(decl.Span)
		}
		if measure != nil {
			measure(128, string(decl.Kind))
			if decl.Import != nil {
				measure(512, decl.Import.Path, decl.Import.Alias, decl.Import.AliasID.Text)
				measure(0, decl.Import.PathSpan.Start.File, decl.Import.PathSpan.End.File, decl.Import.AliasID.Span.Start.File, decl.Import.AliasID.Span.End.File)
			}
			for _, value := range []*ValueDecl{decl.Const, decl.Var} {
				if value == nil {
					continue
				}
				measure(1024)
				for _, name := range value.Names {
					measure(16, name)
				}
				for _, name := range value.NameIDs {
					measure(80, name.Text, name.Span.Start.File, name.Span.End.File)
				}
				for _, pattern := range value.EmbedPatterns {
					measure(16, pattern)
				}
			}
			// Cloning retains every union field, including partial recovery
			// trees. Account for that storage without changing semantic walks.
			for _, value := range []*ValueDecl{decl.Const, decl.Var} {
				if value == nil {
					continue
				}
				walkType(&value.Type)
				for i := range value.Values {
					walkExpr(&value.Values[i])
				}
			}
			if decl.Type != nil {
				measure(1024, decl.Type.Name, decl.Type.NameID.Text, decl.Type.NameID.Span.Start.File, decl.Type.NameID.Span.End.File)
				for i, param := range decl.Type.TypeParams {
					measure(128, param.Name, param.NameID.Text)
					measure(0, param.Span.Start.File, param.Span.End.File, param.NameID.Span.Start.File, param.NameID.Span.End.File)
					walkType(&decl.Type.TypeParams[i].Constraint)
				}
				walkType(&decl.Type.Type)
			}
			walkFunc(decl.Func)
			return
		}
		switch decl.Kind {
		case DeclConst:
			if decl.Const == nil {
				return
			}
			walkType(&decl.Const.Type)
			for i := range decl.Const.Values {
				walkExpr(&decl.Const.Values[i])
			}
		case DeclVar:
			if decl.Var == nil {
				return
			}
			walkType(&decl.Var.Type)
			for i := range decl.Var.Values {
				walkExpr(&decl.Var.Values[i])
			}
		case DeclType:
			if decl.Type == nil {
				return
			}
			for i := range decl.Type.TypeParams {
				walkType(&decl.Type.TypeParams[i].Constraint)
			}
			walkType(&decl.Type.Type)
		case DeclFunc:
			walkFunc(decl.Func)
		}
	}
	walkStmt = func(stmt *Statement) {
		if stmt == nil {
			return
		}
		if visitSpan != nil {
			visitSpan(stmt.Span)
		}
		if measure != nil {
			measure(1024, string(stmt.Kind), stmt.Op, stmt.Label, stmt.LabelID.Text, stmt.TypeSwitchName, stmt.TypeSwitchID.Text)
			measure(0, stmt.LabelID.Span.Start.File, stmt.LabelID.Span.End.File, stmt.TypeSwitchID.Span.Start.File, stmt.TypeSwitchID.Span.End.File)
		}
		for i := range stmt.Decls {
			walkDecl(&stmt.Decls[i])
		}
		walkExpr(stmt.Expr)
		for _, expressions := range [][]Expression{stmt.Left, stmt.Right, stmt.Results} {
			for i := range expressions {
				walkExpr(&expressions[i])
			}
		}
		walkBlock(&stmt.Body)
		walkStmt(stmt.Init)
		walkExpr(stmt.Cond)
		walkStmt(stmt.Post)
		walkStmt(stmt.Else)
		walkExpr(stmt.Key)
		walkExpr(stmt.Value)
		walkExpr(stmt.Range)
		for i := range stmt.Cases {
			if measure != nil {
				measure(256, stmt.Cases[i].Span.Start.File, stmt.Cases[i].Span.End.File)
			}
			for j := range stmt.Cases[i].Values {
				walkExpr(&stmt.Cases[i].Values[j])
			}
			for j := range stmt.Cases[i].Types {
				walkType(&stmt.Cases[i].Types[j])
			}
			walkStmt(stmt.Cases[i].Comm)
			walkBlock(&stmt.Cases[i].Body)
		}
	}
	for i := range program.Files {
		for j := range program.Files[i].Decls {
			walkDecl(&program.Files[i].Decls[j])
		}
	}
}
