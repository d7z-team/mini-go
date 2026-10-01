package lower

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func (l *lowerer) lowerStatement(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	switch stmt.Kind {
	case ast.StmtDecl:
		decls := statementDeclPtrs(&stmt)
		if len(decls) == 0 {
			l.add("hirgen.stmt.decl.missing", "missing declaration statement", stmt.Span)
			return nil, false
		}
		var out []ir.Statement
		for _, decl := range decls {
			lowered, ok := l.lowerLocalDecl(*decl, scope)
			if !ok {
				return nil, false
			}
			out = append(out, lowered...)
		}
		return out, true
	case ast.StmtEmpty:
		return nil, true
	case ast.StmtExpr:
		if stmt.Expr == nil {
			l.add("hirgen.expr.missing", "missing expression", stmt.Span)
			return nil, false
		}
		expr, ok := l.lowerExpression(*stmt.Expr, scope)
		if !ok {
			return nil, false
		}
		return []ir.Statement{{Kind: ir.StmtExpr, Expr: expr}}, true
	case ast.StmtReturn:
		return l.lowerReturn(stmt, scope)
	case ast.StmtAssign:
		if stmt.Op == ":=" {
			return l.lowerShortVarDecl(stmt, scope)
		}
		if isIncDecAssign(stmt.Op) {
			return l.lowerIncDecAssign(stmt, scope)
		}
		if isCompoundAssign(stmt.Op) {
			return l.lowerCompoundAssign(stmt, scope)
		}
		if len(stmt.Right) == 0 {
			l.add("hirgen.assign.shape", "assignment emit requires at least one right-hand expression", stmt.Span)
			return nil, false
		}
		if len(stmt.Right) > 1 {
			return l.lowerMultiValueAssign(stmt, scope)
		}
		if len(stmt.Left) > 1 {
			return l.lowerMultiAssign(stmt, scope)
		}
		if len(stmt.Left) != 1 {
			l.add("hirgen.assign.shape", "assignment emit requires at least one target", stmt.Span)
			return nil, false
		}
		target := stmt.Left[0]
		plan, ok := l.planLValue(target, scope)
		if !ok {
			return nil, false
		}
		value, ok := l.lowerExpressionInType(stmt.Right[0], plan.typ, scope)
		if !ok {
			return nil, false
		}
		if hirResultCount(value) != 1 {
			l.add("hirgen.assign.result", "single assignment expression must produce exactly one result", stmt.Span)
			return nil, false
		}
		if plan.kind == lvalueDiscard {
			return []ir.Statement{{Kind: ir.StmtExpr, Expr: value}}, true
		}
		if plan.kind == lvalueAddress {
			return l.lowerMultiValueAssignThroughTemps([]lvaluePlan{plan}, []ir.Expression{value}, scope), true
		}
		out := append([]ir.Statement(nil), plan.prelude...)
		out = append(out, plan.store(value))
		return out, true
	case ast.StmtIf:
		return l.lowerIf(stmt, scope)
	case ast.StmtFor:
		return l.lowerForWithLabel(stmt, scope, "")
	case ast.StmtRange:
		return l.lowerRangeWithLabel(stmt, scope, "")
	case ast.StmtSwitch:
		return l.lowerSwitchWithLabel(stmt, scope, "")
	case ast.StmtSelect:
		return l.lowerSelectWithLabel(stmt, scope, "")
	case ast.StmtSend:
		if len(stmt.Left) != 1 || len(stmt.Right) != 1 {
			l.add("hirgen.send.shape", "send statement requires channel and value expressions", stmt.Span)
			return nil, false
		}
		valueType, ok := l.sendChannelElementType(stmt.Left[0], scope, "hirgen.send.direction")
		if !ok {
			return nil, false
		}
		channel, ok := l.lowerExpression(stmt.Left[0], scope)
		if !ok {
			return nil, false
		}
		value, ok := l.lowerExpressionInType(stmt.Right[0], valueType, scope)
		if !ok {
			return nil, false
		}
		return []ir.Statement{{Kind: ir.StmtChanSend, Object: channel, Expr: value}}, true
	case ast.StmtBlock:
		return l.lowerBlock(stmt.Body, scope)
	case ast.StmtLabel:
		return l.lowerLabel(stmt, scope)
	case ast.StmtBranch:
		return l.lowerBranch(stmt)
	case ast.StmtPanic:
		if stmt.Expr == nil {
			l.add("hirgen.panic.expr.missing", "panic statement requires an expression", stmt.Span)
			return nil, false
		}
		var expr ir.Expression
		var ok bool
		if isNilLiteral(*stmt.Expr) {
			expr = ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Any"), Value: constant.Scalar("null")}
			ok = true
		} else {
			expr, ok = l.lowerExpression(*stmt.Expr, scope)
		}
		if !ok {
			return nil, false
		}
		return []ir.Statement{
			{Kind: ir.StmtPanic, Expr: expr},
			{Kind: ir.StmtLabel, Label: l.newLabel("panic.after")},
		}, true
	case ast.StmtDefer:
		return l.lowerDefer(stmt, scope)
	case ast.StmtGo:
		return l.lowerGoStatement(stmt, scope)
	default:
		l.add("hirgen.stmt.unsupported", "unsupported AST statement for HIR emit", stmt.Span)
		return nil, false
	}
}
