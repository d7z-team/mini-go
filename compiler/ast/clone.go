package ast

// CloneProgram returns a structurally independent copy of program.
func CloneProgram(program Program) Program {
	out := program
	if program.Files != nil {
		out.Files = make([]File, len(program.Files))
		for i, file := range program.Files {
			out.Files[i] = file
			out.Files[i].Decls = cloneDecls(file.Decls)
		}
	}
	return out
}

// CloneDecl returns a structurally independent copy of decl.
func CloneDecl(decl Decl) Decl {
	out := decl
	if decl.Import != nil {
		value := *decl.Import
		out.Import = &value
	}
	if decl.Const != nil {
		value := cloneValueDecl(*decl.Const)
		out.Const = &value
	}
	if decl.Var != nil {
		value := cloneValueDecl(*decl.Var)
		out.Var = &value
	}
	if decl.Type != nil {
		value := cloneTypeDecl(*decl.Type)
		out.Type = &value
	}
	if decl.Func != nil {
		value := cloneFuncDecl(*decl.Func)
		out.Func = &value
	}
	return out
}

func cloneDecls(values []Decl) []Decl {
	if values == nil {
		return nil
	}
	out := make([]Decl, len(values))
	for i, value := range values {
		out[i] = CloneDecl(value)
	}
	return out
}

func cloneValueDecl(decl ValueDecl) ValueDecl {
	out := decl
	out.Names = append([]string(nil), decl.Names...)
	out.NameIDs = append([]Identifier(nil), decl.NameIDs...)
	out.Type = cloneTypeExpr(decl.Type)
	out.Values = cloneExpressions(decl.Values)
	out.EmbedPatterns = append([]string(nil), decl.EmbedPatterns...)
	return out
}

func cloneTypeDecl(decl TypeDecl) TypeDecl {
	out := decl
	out.TypeParams = cloneTypeParams(decl.TypeParams)
	out.Type = cloneTypeExpr(decl.Type)
	return out
}

func cloneFuncDecl(decl FuncDecl) FuncDecl {
	out := decl
	if decl.Receiver != nil {
		receiver := cloneField(*decl.Receiver)
		out.Receiver = &receiver
	}
	out.TypeParams = cloneTypeParams(decl.TypeParams)
	out.Params = cloneFields(decl.Params)
	out.Results = cloneFields(decl.Results)
	out.Body = cloneBlock(decl.Body)
	return out
}

func cloneTypeParams(values []TypeParam) []TypeParam {
	if values == nil {
		return nil
	}
	out := make([]TypeParam, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Constraint = cloneTypeExpr(value.Constraint)
	}
	return out
}

func cloneField(field Field) Field {
	out := field
	out.Type = cloneTypeExpr(field.Type)
	return out
}

func cloneFields(values []Field) []Field {
	if values == nil {
		return nil
	}
	out := make([]Field, len(values))
	for i, value := range values {
		out[i] = cloneField(value)
	}
	return out
}

func cloneTypeExpr(typ TypeExpr) TypeExpr {
	out := typ
	if typ.Elem != nil {
		elem := cloneTypeExpr(*typ.Elem)
		out.Elem = &elem
	}
	if typ.Key != nil {
		key := cloneTypeExpr(*typ.Key)
		out.Key = &key
	}
	if typ.Len != nil {
		length := CloneExpression(*typ.Len)
		out.Len = &length
	}
	out.Params = cloneFields(typ.Params)
	out.Results = cloneFields(typ.Results)
	out.Fields = cloneFields(typ.Fields)
	if typ.Methods != nil {
		out.Methods = make([]FuncDecl, len(typ.Methods))
		for i, method := range typ.Methods {
			out.Methods[i] = cloneFuncDecl(method)
		}
	}
	if typ.Embeds != nil {
		out.Embeds = make([]TypeExpr, len(typ.Embeds))
		for i, embed := range typ.Embeds {
			out.Embeds[i] = cloneTypeExpr(embed)
		}
	}
	if typ.Terms != nil {
		out.Terms = make([]TypeTerm, len(typ.Terms))
		for i, term := range typ.Terms {
			out.Terms[i] = term
			out.Terms[i].Type = cloneTypeExpr(term.Type)
		}
	}
	if typ.Base != nil {
		base := cloneTypeExpr(*typ.Base)
		out.Base = &base
	}
	if typ.TypeArgs != nil {
		out.TypeArgs = make([]TypeExpr, len(typ.TypeArgs))
		for i, arg := range typ.TypeArgs {
			out.TypeArgs[i] = cloneTypeExpr(arg)
		}
	}
	return out
}

func cloneBlock(block BlockStmt) BlockStmt {
	out := block
	if block.Stmts != nil {
		out.Stmts = make([]Statement, len(block.Stmts))
		for i, statement := range block.Stmts {
			out.Stmts[i] = cloneStatement(statement)
		}
	}
	return out
}

func cloneStatement(statement Statement) Statement {
	out := statement
	out.Decls = cloneDecls(statement.Decls)
	if statement.Expr != nil {
		expr := CloneExpression(*statement.Expr)
		out.Expr = &expr
	}
	out.Left = cloneExpressions(statement.Left)
	out.Right = cloneExpressions(statement.Right)
	out.Body = cloneBlock(statement.Body)
	if statement.Init != nil {
		init := cloneStatement(*statement.Init)
		out.Init = &init
	}
	if statement.Cond != nil {
		condition := CloneExpression(*statement.Cond)
		out.Cond = &condition
	}
	if statement.Post != nil {
		post := cloneStatement(*statement.Post)
		out.Post = &post
	}
	if statement.Else != nil {
		elseStatement := cloneStatement(*statement.Else)
		out.Else = &elseStatement
	}
	if statement.Key != nil {
		key := CloneExpression(*statement.Key)
		out.Key = &key
	}
	if statement.Value != nil {
		value := CloneExpression(*statement.Value)
		out.Value = &value
	}
	if statement.Range != nil {
		rangeExpr := CloneExpression(*statement.Range)
		out.Range = &rangeExpr
	}
	if statement.Cases != nil {
		out.Cases = make([]CaseClause, len(statement.Cases))
		for i, clause := range statement.Cases {
			out.Cases[i] = cloneCaseClause(clause)
		}
	}
	out.Results = cloneExpressions(statement.Results)
	return out
}

func cloneCaseClause(clause CaseClause) CaseClause {
	out := clause
	out.Values = cloneExpressions(clause.Values)
	if clause.Types != nil {
		out.Types = make([]TypeExpr, len(clause.Types))
		for i, typ := range clause.Types {
			out.Types[i] = cloneTypeExpr(typ)
		}
	}
	if clause.Comm != nil {
		comm := cloneStatement(*clause.Comm)
		out.Comm = &comm
	}
	out.Body = cloneBlock(clause.Body)
	return out
}

// CloneExpression returns a structurally independent copy of expr.
func CloneExpression(expr Expression) Expression {
	out := expr
	if expr.Type != nil {
		typ := cloneTypeExpr(*expr.Type)
		out.Type = &typ
	}
	clone := func(value *Expression) *Expression {
		if value == nil {
			return nil
		}
		out := CloneExpression(*value)
		return &out
	}
	out.Left = clone(expr.Left)
	out.Right = clone(expr.Right)
	out.Operand = clone(expr.Operand)
	out.Callee = clone(expr.Callee)
	out.Args = cloneExpressions(expr.Args)
	out.Index = clone(expr.Index)
	out.Start = clone(expr.Start)
	out.End = clone(expr.End)
	out.Max = clone(expr.Max)
	out.Items = cloneKeyValues(expr.Items)
	if expr.EmbedFiles != nil {
		out.EmbedFiles = make([]EmbedFile, len(expr.EmbedFiles))
		for i, file := range expr.EmbedFiles {
			out.EmbedFiles[i] = file
			out.EmbedFiles[i].Data = append([]byte(nil), file.Data...)
		}
	}
	if expr.Func != nil {
		function := cloneFuncDecl(*expr.Func)
		out.Func = &function
	}
	return out
}

func cloneExpressions(values []Expression) []Expression {
	if values == nil {
		return nil
	}
	out := make([]Expression, len(values))
	for i, value := range values {
		out[i] = CloneExpression(value)
	}
	return out
}

func cloneKeyValues(values []KeyValue) []KeyValue {
	if values == nil {
		return nil
	}
	out := make([]KeyValue, len(values))
	for i, value := range values {
		out[i] = value
		if value.Key != nil {
			key := CloneExpression(*value.Key)
			out[i].Key = &key
		}
		out[i].Value = CloneExpression(value.Value)
	}
	return out
}
