package ast

type NameRole uint8

const (
	NameInvalid NameRole = iota
	NameDefinition
	NameReference
	NameSelector
	NameImport
	NameLabelDefinition
	NameLabelReference
)

type NameOccurrence struct {
	Name Identifier
	Node NodeID
	Role NameRole
}

func Names(program Program) []NameOccurrence {
	var out []NameOccurrence
	add := func(name Identifier, node NodeID, role NameRole) {
		if name.ID != 0 && name.Text != "" && name.Span.Valid() {
			out = append(out, NameOccurrence{Name: name, Node: node, Role: role})
		}
	}
	for i := range program.Files {
		add(program.Files[i].PackageID, program.Files[i].NodeID, NameDefinition)
		for j := range program.Files[i].Decls {
			collectDeclNames(&program.Files[i].Decls[j], add)
		}
	}
	return out
}

func collectDeclNames(decl *Decl, add func(Identifier, NodeID, NameRole)) {
	if !decl.hasPayload() {
		return
	}
	switch decl.Kind {
	case DeclImport:
		add(decl.Import.AliasID, decl.NodeID, NameImport)
	case DeclConst:
		collectValueNames(decl.Const, decl.NodeID, add)
	case DeclVar:
		collectValueNames(decl.Var, decl.NodeID, add)
	case DeclType:
		add(decl.Type.NameID, decl.NodeID, NameDefinition)
		for i := range decl.Type.TypeParams {
			collectTypeParamNames(&decl.Type.TypeParams[i], add)
		}
		collectTypeNames(&decl.Type.Type, add)
	case DeclFunc:
		collectFuncNamesAt(decl.Func, decl.NodeID, add)
	}
}

func collectValueNames(decl *ValueDecl, node NodeID, add func(Identifier, NodeID, NameRole)) {
	for _, name := range decl.NameIDs {
		add(name, node, NameDefinition)
	}
	collectTypeNames(&decl.Type, add)
	for i := range decl.Values {
		collectExprNames(&decl.Values[i], add)
	}
}

func collectFuncNames(decl *FuncDecl, add func(Identifier, NodeID, NameRole)) {
	collectFuncNamesAt(decl, decl.NodeID, add)
}

func collectFuncNamesAt(decl *FuncDecl, nameNode NodeID, add func(Identifier, NodeID, NameRole)) {
	add(decl.NameID, nameNode, NameDefinition)
	if decl.Receiver != nil {
		collectFieldNames(decl.Receiver, add)
	}
	for i := range decl.TypeParams {
		collectTypeParamNames(&decl.TypeParams[i], add)
	}
	for i := range decl.Params {
		collectFieldNames(&decl.Params[i], add)
	}
	for i := range decl.Results {
		collectFieldNames(&decl.Results[i], add)
	}
	collectBlockNames(&decl.Body, add)
}

func collectTypeParamNames(param *TypeParam, add func(Identifier, NodeID, NameRole)) {
	add(param.NameID, param.NodeID, NameDefinition)
	collectTypeNames(&param.Constraint, add)
}

func collectFieldNames(field *Field, add func(Identifier, NodeID, NameRole)) {
	add(field.NameID, field.NodeID, NameDefinition)
	collectTypeNames(&field.Type, add)
}

func collectTypeNames(typ *TypeExpr, add func(Identifier, NodeID, NameRole)) {
	if typ == nil || typ.Kind == TypeInvalid {
		return
	}
	add(typ.QualifierID, typ.NodeID, NameReference)
	if typ.QualifierID.ID != 0 {
		add(typ.NameID, typ.NodeID, NameSelector)
	} else {
		add(typ.NameID, typ.NodeID, NameReference)
	}
	collectTypeNames(typ.Base, add)
	for i := range typ.TypeArgs {
		collectTypeNames(&typ.TypeArgs[i], add)
	}
	collectTypeNames(typ.Elem, add)
	collectTypeNames(typ.Key, add)
	if typ.Len != nil {
		collectExprNames(typ.Len, add)
	}
	for i := range typ.Params {
		collectFieldNames(&typ.Params[i], add)
	}
	for i := range typ.Results {
		collectFieldNames(&typ.Results[i], add)
	}
	for i := range typ.Fields {
		collectFieldNames(&typ.Fields[i], add)
	}
	for i := range typ.Methods {
		collectFuncNames(&typ.Methods[i], add)
	}
	for i := range typ.Embeds {
		collectTypeNames(&typ.Embeds[i], add)
	}
	for i := range typ.Terms {
		collectTypeNames(&typ.Terms[i].Type, add)
	}
}

func collectBlockNames(block *BlockStmt, add func(Identifier, NodeID, NameRole)) {
	if block == nil {
		return
	}
	for i := range block.Stmts {
		collectStatementNames(&block.Stmts[i], add)
	}
}

func collectStatementNames(stmt *Statement, add func(Identifier, NodeID, NameRole)) {
	if stmt == nil {
		return
	}
	if stmt.Kind == StmtLabel {
		add(stmt.LabelID, stmt.NodeID, NameLabelDefinition)
	} else {
		add(stmt.LabelID, stmt.NodeID, NameLabelReference)
	}
	add(stmt.TypeSwitchID, stmt.NodeID, NameDefinition)
	for i := range stmt.Decls {
		collectDeclNames(&stmt.Decls[i], add)
	}
	if stmt.Expr != nil {
		collectExprNames(stmt.Expr, add)
	}
	for i := range stmt.Left {
		if stmt.Kind == StmtAssign && stmt.Op == ":=" && stmt.Left[i].Kind == ExprIdent {
			add(stmt.Left[i].NameID, stmt.Left[i].NodeID, NameDefinition)
		} else {
			collectExprNames(&stmt.Left[i], add)
		}
	}
	for i := range stmt.Right {
		collectExprNames(&stmt.Right[i], add)
	}
	collectBlockNames(&stmt.Body, add)
	collectStatementNames(stmt.Init, add)
	if stmt.Cond != nil {
		collectExprNames(stmt.Cond, add)
	}
	collectStatementNames(stmt.Post, add)
	collectStatementNames(stmt.Else, add)
	for _, target := range []*Expression{stmt.Key, stmt.Value} {
		if target == nil {
			continue
		}
		if stmt.Kind == StmtRange && stmt.Op == ":=" && target.Kind == ExprIdent {
			add(target.NameID, target.NodeID, NameDefinition)
		} else {
			collectExprNames(target, add)
		}
	}
	if stmt.Range != nil {
		collectExprNames(stmt.Range, add)
	}
	for i := range stmt.Cases {
		clause := &stmt.Cases[i]
		for j := range clause.Values {
			collectExprNames(&clause.Values[j], add)
		}
		for j := range clause.Types {
			collectTypeNames(&clause.Types[j], add)
		}
		collectStatementNames(clause.Comm, add)
		collectBlockNames(&clause.Body, add)
	}
	for i := range stmt.Results {
		collectExprNames(&stmt.Results[i], add)
	}
}

func collectExprNames(expr *Expression, add func(Identifier, NodeID, NameRole)) {
	if expr == nil || expr.Kind == ExprInvalid {
		return
	}
	if expr.Kind == ExprSelector {
		add(expr.NameID, expr.NodeID, NameSelector)
	} else {
		add(expr.NameID, expr.NodeID, NameReference)
	}
	collectTypeNames(expr.Type, add)
	collectExprNames(expr.Left, add)
	collectExprNames(expr.Right, add)
	collectExprNames(expr.Operand, add)
	collectExprNames(expr.Callee, add)
	for i := range expr.Args {
		collectExprNames(&expr.Args[i], add)
	}
	collectExprNames(expr.Index, add)
	collectExprNames(expr.Start, add)
	collectExprNames(expr.End, add)
	collectExprNames(expr.Max, add)
	for i := range expr.Items {
		collectExprNames(expr.Items[i].Key, add)
		collectExprNames(&expr.Items[i].Value, add)
	}
	if expr.Kind == ExprFunc && expr.Func != nil {
		collectFuncNames(expr.Func, add)
	}
}
