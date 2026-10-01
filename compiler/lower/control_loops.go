package lower

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func (l *lowerer) lowerIf(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	if stmt.Cond == nil {
		l.add("hirgen.if.cond.missing", "missing if condition", stmt.Span)
		return nil, false
	}
	ifScope := l.childScope(scope)
	elseLabel := l.newLabel("if.else")
	endLabel := l.newLabel("if.end")
	var out []ir.Statement
	if stmt.Init != nil {
		init, ok := l.lowerStatement(*stmt.Init, ifScope)
		if !ok {
			return nil, false
		}
		out = append(out, init...)
	}
	cond, ok := l.lowerExpression(*stmt.Cond, ifScope)
	if !ok {
		return nil, false
	}
	falseLabel := endLabel
	if stmt.Else != nil {
		falseLabel = elseLabel
	}
	out = append(out, ir.Statement{Kind: ir.StmtJumpIf, Expr: cond, Label: falseLabel, BranchNegated: true})
	body, ok := l.lowerBlock(stmt.Body, ifScope)
	if !ok {
		return nil, false
	}
	out = append(out, body...)
	if stmt.Else != nil {
		out = append(out, ir.Statement{Kind: ir.StmtJump, Label: endLabel}, ir.Statement{Kind: ir.StmtLabel, Label: elseLabel})
		elseBody, ok := l.lowerStatement(*stmt.Else, ifScope)
		if !ok {
			return nil, false
		}
		out = append(out, elseBody...)
	}
	out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: endLabel})
	return out, true
}

func (l *lowerer) lowerForWithLabel(stmt ast.Statement, scope *funcScope, userLabel string) ([]ir.Statement, bool) {
	loopScope := l.childScope(scope)
	condLabel := l.newLabel("for.cond")
	postLabel := l.newLabel("for.post")
	endLabel := l.newLabel("for.end")
	var out []ir.Statement
	if stmt.Init != nil {
		init, ok := l.lowerStatement(*stmt.Init, loopScope)
		if !ok {
			return nil, false
		}
		out = append(out, init...)
	}
	out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: condLabel})
	if stmt.Cond != nil {
		cond, ok := l.lowerExpression(*stmt.Cond, loopScope)
		if !ok {
			return nil, false
		}
		out = append(out, ir.Statement{Kind: ir.StmtJumpIf, Expr: cond, Label: endLabel, BranchNegated: true})
	}
	l.pushBranchWithUserLabel(userLabel, endLabel, postLabel)
	body, ok := l.lowerBlock(stmt.Body, loopScope)
	l.popBranch()
	if !ok {
		return nil, false
	}
	out = append(out, body...)
	out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: postLabel})
	if stmt.Post != nil {
		post, ok := l.lowerStatement(*stmt.Post, loopScope)
		if !ok {
			return nil, false
		}
		out = append(out, post...)
	}
	out = append(out,
		ir.Statement{Kind: ir.StmtJump, Label: condLabel},
		ir.Statement{Kind: ir.StmtLabel, Label: endLabel},
	)
	return out, true
}
