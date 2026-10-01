package ast

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/source"
)

func validateBlock(block BlockStmt, add func(string, string, source.Span)) {
	for _, stmt := range block.Stmts {
		validateStatement(stmt, add)
	}
}

func validateStatement(stmt Statement, add func(string, string, source.Span)) {
	switch stmt.Kind {
	case StmtInvalid:
	case StmtEmpty:
	case StmtDecl:
		decls := statementDecls(stmt)
		if len(decls) == 0 {
			add("ast.stmt.decl.missing", "missing declaration statement payload", stmt.Span)
			return
		}
		for _, decl := range decls {
			validateDecl(decl, map[string]struct{}{}, add)
		}
	case StmtExpr, StmtDefer, StmtGo, StmtPanic:
		if stmt.Expr == nil {
			add("ast.stmt.expr.missing", "missing expression", stmt.Span)
			return
		}
		validateExpression(*stmt.Expr, add)
	case StmtAssign:
		if isIncDecAssign(stmt.Op) {
			if len(stmt.Left) != 1 || len(stmt.Right) != 0 {
				add("ast.stmt.assign.incdec.shape", "increment/decrement assignment requires one target and no right expressions", stmt.Span)
			}
			for _, expr := range stmt.Left {
				validateExpression(expr, add)
			}
			return
		}
		if len(stmt.Left) == 0 || len(stmt.Right) == 0 {
			add("ast.stmt.assign.empty", "assignment requires left and right expressions", stmt.Span)
		}
		for _, expr := range stmt.Left {
			validateExpression(expr, add)
		}
		for _, expr := range stmt.Right {
			validateExpression(expr, add)
		}
	case StmtReturn:
		for _, expr := range stmt.Results {
			validateExpression(expr, add)
		}
	case StmtIf, StmtFor:
		if stmt.Cond != nil {
			validateExpression(*stmt.Cond, add)
		}
		if stmt.Init != nil {
			validateStatement(*stmt.Init, add)
		}
		if stmt.Post != nil {
			validateStatement(*stmt.Post, add)
		}
		validateBlock(stmt.Body, add)
		if stmt.Else != nil {
			validateStatement(*stmt.Else, add)
		}
	case StmtRange:
		if stmt.Range == nil {
			add("ast.stmt.range.expr.missing", "missing range expression", stmt.Span)
		} else {
			validateExpression(*stmt.Range, add)
		}
		if stmt.Key != nil {
			validateExpression(*stmt.Key, add)
		}
		if stmt.Value != nil {
			validateExpression(*stmt.Value, add)
		}
		validateBlock(stmt.Body, add)
	case StmtSwitch, StmtSelect:
		if stmt.Expr != nil {
			validateExpression(*stmt.Expr, add)
		}
		for _, clause := range stmt.Cases {
			for _, expr := range clause.Values {
				validateExpression(expr, add)
			}
			for _, typ := range clause.Types {
				validateTypeExpr(typ, clause.Span, add)
			}
			if clause.Comm != nil {
				validateStatement(*clause.Comm, add)
			}
			validateBlock(clause.Body, add)
		}
	case StmtSend:
		if len(stmt.Left) != 1 || len(stmt.Right) != 1 {
			add("ast.stmt.send.shape", "send statement requires channel and value expressions", stmt.Span)
			return
		}
		validateExpression(stmt.Left[0], add)
		validateExpression(stmt.Right[0], add)
	case StmtBlock:
		validateBlock(stmt.Body, add)
	case StmtLabel:
		if strings.TrimSpace(stmt.Label) == "" {
			add("ast.stmt.label.missing", "missing statement label", stmt.Span)
		}
		validateBlock(stmt.Body, add)
	case StmtBranch:
		if strings.TrimSpace(stmt.Op) == "" {
			add("ast.stmt.branch.op.missing", "missing branch operator", stmt.Span)
		}
	default:
		add("ast.stmt.kind.unknown", "unknown statement kind", stmt.Span)
	}
}

func statementDecls(stmt Statement) []Decl {
	return stmt.Decls
}

func isIncDecAssign(operator string) bool {
	return operator == "++" || operator == "--"
}

func validateTypeExpr(typ TypeExpr, span source.Span, add func(string, string, source.Span)) {
	switch typ.Kind {
	case TypeName:
		if strings.TrimSpace(typ.Name) == "" {
			add("ast.type.name.missing", "missing type name", span)
		}
	case TypeArray, TypeSlice, TypePointer, TypeChan:
		if typ.Elem == nil {
			add("ast.type.elem.missing", "missing element type", span)
			return
		}
		validateTypeExpr(*typ.Elem, span, add)
	case TypeMap:
		if typ.Key == nil || typ.Elem == nil {
			add("ast.type.map.missing", "missing map key or element type", span)
			return
		}
		validateTypeExpr(*typ.Key, span, add)
		validateTypeExpr(*typ.Elem, span, add)
	case TypeInstance:
		if typ.Base == nil || len(typ.TypeArgs) == 0 {
			add("ast.type.instance.missing", "type instantiation requires a base and type arguments", span)
			return
		}
		validateTypeExpr(*typ.Base, span, add)
		for _, arg := range typ.TypeArgs {
			validateTypeExpr(arg, arg.Span, add)
		}
	case TypeFunc:
		for i, param := range typ.Params {
			if param.Variadic && i != len(typ.Params)-1 {
				add("ast.type.func.param.variadic.position", "variadic parameter must be last", param.Span)
			}
			validateTypeExpr(param.Type, param.Span, add)
		}
		for _, result := range typ.Results {
			if result.Variadic {
				add("ast.type.func.result.variadic", "function result cannot be variadic", result.Span)
			}
			validateTypeExpr(result.Type, result.Span, add)
		}
	case TypeStruct:
		for _, field := range typ.Fields {
			validateTypeExpr(field.Type, field.Span, add)
		}
	case TypeInterface:
		for _, embed := range typ.Embeds {
			validateTypeExpr(embed, span, add)
		}
		for _, term := range typ.Terms {
			validateTypeExpr(term.Type, term.Span, add)
		}
		methodNames := map[string]struct{}{}
		for _, method := range typ.Methods {
			name := strings.TrimSpace(method.Name)
			if name == "" {
				add("ast.type.interface.method.name.missing", "interface method requires a name", typ.Span)
			} else if name == "_" {
				add("ast.type.interface.method.blank", "interface method name cannot be blank", typ.Span)
			} else if _, exists := methodNames[name]; exists {
				add("ast.type.interface.method.duplicate", "interface declares a method more than once", typ.Span)
			} else {
				methodNames[name] = struct{}{}
			}
			validateFuncSignature(method, add)
		}
	case TypeInvalid:
		add("ast.type.kind.missing", "missing type kind", span)
	default:
		add("ast.type.kind.unknown", "unknown type kind", span)
	}
}

func validateExpression(expr Expression, add func(string, string, source.Span)) {
	switch expr.Kind {
	case ExprIdent:
		if strings.TrimSpace(expr.Name) == "" {
			add("ast.expr.ident.missing", "missing identifier name", expr.Span)
		}
	case ExprLiteral:
		if strings.TrimSpace(expr.Literal) == "" {
			add("ast.expr.literal.missing", "missing literal text", expr.Span)
		}
	case ExprUnary, ExprAddr, ExprDeref, ExprReceive:
		if expr.Operand == nil {
			add("ast.expr.operand.missing", "missing operand", expr.Span)
			return
		}
		validateExpression(*expr.Operand, add)
	case ExprBinary:
		if expr.Left == nil || expr.Right == nil {
			add("ast.expr.binary.missing", "missing binary operand", expr.Span)
			return
		}
		validateExpression(*expr.Left, add)
		validateExpression(*expr.Right, add)
	case ExprCall:
		if expr.Callee == nil {
			add("ast.expr.call.callee.missing", "missing call callee", expr.Span)
			return
		}
		validateExpression(*expr.Callee, add)
		for _, arg := range expr.Args {
			validateExpression(arg, add)
		}
	case ExprSelector:
		if expr.Operand == nil || strings.TrimSpace(expr.Field) == "" {
			add("ast.expr.selector.missing", "missing selector operand or field", expr.Span)
			return
		}
		validateExpression(*expr.Operand, add)
	case ExprIndex, ExprIndexList, ExprSlice:
		if expr.Operand == nil {
			add("ast.expr.index.operand.missing", "missing indexed operand", expr.Span)
			return
		}
		validateExpression(*expr.Operand, add)
		if expr.Index != nil {
			validateExpression(*expr.Index, add)
		}
		if expr.Start != nil {
			validateExpression(*expr.Start, add)
		}
		if expr.End != nil {
			validateExpression(*expr.End, add)
		}
		if expr.Max != nil {
			validateExpression(*expr.Max, add)
		}
		for _, arg := range expr.Args {
			validateExpression(arg, add)
		}
	case ExprComposite:
		if expr.Type != nil && expr.Type.Kind != TypeInvalid {
			validateTypeExpr(*expr.Type, expr.Span, add)
		}
		for _, item := range expr.Items {
			if item.Key != nil {
				validateExpression(*item.Key, add)
			}
			validateExpression(item.Value, add)
		}
	case ExprFunc:
		if expr.Func == nil {
			add("ast.expr.func.missing", "function expression requires a function body", expr.Span)
		} else {
			validateFuncDecl(*expr.Func, expr.Span, add)
		}
	case ExprEmbed:
		if len(expr.EmbedFiles) == 0 {
			add("ast.expr.embed.files_missing", "embed initializer requires at least one file", expr.Span)
		}
	case ExprConvert, ExprAssert:
		if expr.Type == nil {
			add("ast.type.kind.missing", "missing type kind", expr.Span)
		} else {
			validateTypeExpr(*expr.Type, expr.Span, add)
		}
		if expr.Operand == nil {
			add("ast.expr.convert.operand.missing", "missing conversion/assert operand", expr.Span)
			return
		}
		validateExpression(*expr.Operand, add)
	case ExprInvalid:
		add("ast.expr.kind.missing", "missing expression kind", expr.Span)
	default:
		add("ast.expr.kind.unknown", "unknown expression kind", expr.Span)
	}
}
