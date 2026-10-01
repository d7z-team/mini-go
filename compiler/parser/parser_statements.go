package parser

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/token"
)

func (p *parser) parseBlock() ast.BlockStmt {
	start := p.expect(token.Lbrace, "parser.block", "expected {")
	var statements []ast.Statement
	for !p.at(token.Rbrace) && !p.at(token.EOF) {
		before := p.pos
		p.consumeSemicolons()
		if p.at(token.Rbrace) {
			break
		}
		statements = append(statements, p.parseStatement())
		p.consumeSemicolons()
		p.ensureProgress(before, "parser.progress.block", stmtStart)
	}
	end := p.expect(token.Rbrace, "parser.block", "expected }")
	return ast.BlockStmt{Span: join(start, end), Stmts: statements}
}

func (p *parser) parseStatement() ast.Statement {
	switch p.current().Kind {
	case token.Const, token.Var, token.Type:
		decls := p.parseDecl(false)
		if len(decls) == 0 {
			return ast.Statement{Kind: ast.StmtEmpty, Span: p.current().Span}
		}
		span := decls[0].Span
		if len(decls) > 1 {
			span = spanJoin(decls[0].Span, decls[len(decls)-1].Span)
		}
		return ast.Statement{Kind: ast.StmtDecl, Span: span, Decls: decls}
	case token.Return:
		return p.parseReturnStmt()
	case token.If:
		return p.parseIfStmt()
	case token.For:
		return p.parseForStmt()
	case token.Switch:
		return p.parseSwitchStmt()
	case token.Select:
		return p.parseSelectStmt()
	case token.Defer:
		return p.parseUnaryCallStmt(ast.StmtDefer, "parser.defer", "expected deferred call")
	case token.Go:
		return p.parseUnaryCallStmt(ast.StmtGo, "parser.go", "expected go call")
	case token.Break, token.Continue, token.Goto, token.Fallthrough:
		return p.parseBranchStmt()
	case token.Lbrace:
		block := p.parseBlock()
		return ast.Statement{Kind: ast.StmtBlock, Span: block.Span, Body: block}
	case token.Ident:
		if p.peek(1).Kind == token.Colon {
			label := p.advance()
			p.advance()
			stmt := ast.Statement{Kind: ast.StmtLabel, Span: spanJoin(startSpan(label), p.previous().Span), Label: label.Lexeme, LabelID: identifier(label)}
			if !p.at(token.Rbrace) && !p.at(token.EOF) {
				nested := p.parseStatement()
				stmt.Body = ast.BlockStmt{Stmts: []ast.Statement{nested}}
				stmt.Span = spanJoin(startSpan(label), nested.Span)
			}
			return stmt
		}
	}
	return p.parseSimpleStmt()
}

func (p *parser) parseReturnStmt() ast.Statement {
	start := p.expect(token.Return, "parser.return", "expected return")
	results := p.parseExpressionListUntil(token.Semicolon, token.Rbrace, token.EOF)
	out := ast.Statement{Kind: ast.StmtReturn, Span: startSpan(start), Results: results}
	if len(results) != 0 {
		out.Span = exprSpan(start, results[len(results)-1])
	}
	return out
}

func (p *parser) parseUnaryCallStmt(kind ast.StmtKind, code, message string) ast.Statement {
	start := p.advance()
	parenthesizedWholeCall := p.startsParenthesizedWholeExpression()
	parenthesizedSpan := p.current().Span
	expr := p.parseExpression(1)
	if parenthesizedWholeCall && expr.Kind == ast.ExprCall {
		p.add(code+".parenthesized", message+" cannot be parenthesized", parenthesizedSpan)
	}
	if expr.Kind != ast.ExprCall {
		p.add(code, message, expr.Span)
	}
	return ast.Statement{Kind: kind, Span: exprSpan(start, expr), Expr: &expr}
}

func (p *parser) startsParenthesizedWholeExpression() bool {
	if !p.at(token.Lparen) {
		return false
	}
	depth := 0
	for index := p.pos; index < p.lexical.TokenCount(); index++ {
		switch p.lexical.TokenKind(index) {
		case token.Lparen:
			depth++
		case token.Rparen:
			depth--
			if depth == 0 {
				switch p.peek(index - p.pos + 1).Kind {
				case token.Semicolon, token.Rbrace, token.EOF:
					return true
				default:
					return false
				}
			}
		case token.EOF:
			return false
		}
	}
	return false
}

func (p *parser) parseBranchStmt() ast.Statement {
	start := p.advance()
	label := ""
	var labelID ast.Identifier
	if p.at(token.Ident) {
		labelToken := p.advance()
		label = labelToken.Lexeme
		labelID = identifier(labelToken)
	}
	return ast.Statement{Kind: ast.StmtBranch, Span: spanJoin(startSpan(start), p.previous().Span), Op: strings.ToLower(string(start.Kind)), Label: label, LabelID: labelID}
}

func (p *parser) parseIfStmt() ast.Statement {
	start := p.expect(token.If, "parser.if", "expected if")
	init, cond := p.parseOptionalInitAndCondition(token.Lbrace)
	body := p.parseBlock()
	var elseStmt *ast.Statement
	if p.match(token.Else) {
		if p.at(token.If) {
			next := p.parseIfStmt()
			elseStmt = &next
		} else {
			block := p.parseBlock()
			next := ast.Statement{Kind: ast.StmtBlock, Span: block.Span, Body: block}
			elseStmt = &next
		}
	}
	return ast.Statement{Kind: ast.StmtIf, Span: spanJoin(startSpan(start), body.Span), Init: init, Cond: cond, Body: body, Else: elseStmt}
}

func (p *parser) parseForStmt() ast.Statement {
	start := p.expect(token.For, "parser.for", "expected for")
	if p.at(token.Lbrace) {
		body := p.parseBlock()
		return ast.Statement{Kind: ast.StmtFor, Span: spanJoin(startSpan(start), body.Span), Body: body}
	}
	first := p.parseSimpleStmtBefore(token.Semicolon, token.Lbrace)
	if first.Kind == ast.StmtRange && p.at(token.Lbrace) {
		first.Body = p.parseBlock()
		first.Span = spanJoin(startSpan(start), first.Body.Span)
		return first
	}
	if p.match(token.Semicolon) {
		init := &first
		var cond *ast.Expression
		if !p.at(token.Semicolon) {
			expr := p.parseExpressionBeforeBlock()
			cond = &expr
		}
		p.expect(token.Semicolon, "parser.for", "expected ; in for clause")
		var post *ast.Statement
		if !p.at(token.Lbrace) {
			parsed := p.parseSimpleStmtBefore(token.Lbrace)
			if parsed.Kind == ast.StmtAssign && parsed.Op == ":=" {
				p.add("parser.for.post.short_decl", "for post statement must not be a short variable declaration", parsed.Span)
			}
			post = &parsed
		}
		body := p.parseBlock()
		return ast.Statement{Kind: ast.StmtFor, Span: spanJoin(startSpan(start), body.Span), Init: init, Cond: cond, Post: post, Body: body}
	}
	var cond *ast.Expression
	if first.Expr != nil && first.Kind == ast.StmtExpr {
		cond = first.Expr
	}
	body := p.parseBlock()
	return ast.Statement{Kind: ast.StmtFor, Span: spanJoin(startSpan(start), body.Span), Cond: cond, Body: body}
}

func (p *parser) parseSwitchStmt() ast.Statement {
	start := p.expect(token.Switch, "parser.switch", "expected switch")
	init, expr := p.parseOptionalInitAndCondition(token.Lbrace)
	typeSwitch := false
	typeSwitchName := ""
	var typeSwitchID ast.Identifier
	if expr != nil && expr.Kind == ast.ExprAssert && expr.TypeSwitch {
		typeSwitch = true
		expr = expr.Operand
	} else if init != nil && init.Kind == ast.StmtAssign && len(init.Right) == 1 && init.Right[0].Kind == ast.ExprAssert && init.Right[0].TypeSwitch {
		typeSwitch = true
		if init.Op != ":=" {
			p.add("parser.typeswitch.guard.assign", "type switch guard must use :=", init.Span)
		} else if len(init.Left) != 1 || init.Left[0].Kind != ast.ExprIdent {
			p.add("parser.typeswitch.guard.binding", "type switch guard binding must be a single identifier", init.Span)
		} else if init.Left[0].Name == "_" {
			p.add("parser.typeswitch.guard.blank", "type switch guard binding cannot be _", init.Left[0].Span)
		} else {
			typeSwitchName = init.Left[0].Name
			typeSwitchID = init.Left[0].NameID
		}
		expr = init.Right[0].Operand
		init = nil
	}
	p.expect(token.Lbrace, "parser.switch", "expected { after switch")
	var cases []ast.CaseClause
	for !p.at(token.Rbrace) && !p.at(token.EOF) {
		before := p.pos
		cases = append(cases, p.parseCaseClause(typeSwitch))
		p.ensureProgress(before, "parser.progress.switch", stmtStart)
	}
	end := p.expect(token.Rbrace, "parser.switch", "expected } after switch")
	return ast.Statement{Kind: ast.StmtSwitch, Span: join(start, end), Init: init, Expr: expr, Cases: cases, TypeSwitch: typeSwitch, TypeSwitchName: typeSwitchName, TypeSwitchID: typeSwitchID}
}

func (p *parser) parseCaseClause(typeSwitch bool) ast.CaseClause {
	start := p.current()
	if p.match(token.Default) {
		p.expect(token.Colon, "parser.case", "expected : after default")
		return ast.CaseClause{Span: startSpan(start), Default: true, Body: p.parseCaseBody()}
	}
	p.expect(token.Case, "parser.case", "expected case")
	clause := ast.CaseClause{Span: startSpan(start)}
	nilSeen := false
	if typeSwitch {
		for {
			if p.matchIdent("nil") {
				if nilSeen {
					p.add("parser.typeswitch.nil.duplicate", "type switch cannot contain duplicate nil cases", p.previous().Span)
				}
				nilSeen = true
				clause.Nil = true
			} else {
				clause.Types = append(clause.Types, p.parseType())
			}
			if !p.match(token.Comma) {
				break
			}
		}
	} else {
		clause.Values = p.parseExpressionListUntil(token.Colon)
	}
	p.expect(token.Colon, "parser.case", "expected : after case")
	clause.Body = p.parseCaseBody()
	return clause
}

func (p *parser) parseCaseBody() ast.BlockStmt {
	var statements []ast.Statement
	for !p.at(token.Case) && !p.at(token.Default) && !p.at(token.Rbrace) && !p.at(token.EOF) {
		before := p.pos
		p.consumeSemicolons()
		if p.at(token.Case) || p.at(token.Default) || p.at(token.Rbrace) {
			break
		}
		statements = append(statements, p.parseStatement())
		p.consumeSemicolons()
		p.ensureProgress(before, "parser.progress.case_body", stmtStart)
	}
	return ast.BlockStmt{Stmts: statements}
}

func (p *parser) parseSelectStmt() ast.Statement {
	start := p.expect(token.Select, "parser.select", "expected select")
	p.expect(token.Lbrace, "parser.select", "expected { after select")
	var cases []ast.CaseClause
	for !p.at(token.Rbrace) && !p.at(token.EOF) {
		before := p.pos
		caseStart := p.current()
		if p.match(token.Default) {
			p.expect(token.Colon, "parser.select", "expected : after default")
			cases = append(cases, ast.CaseClause{Span: startSpan(caseStart), Default: true, Body: p.parseCaseBody()})
			p.ensureProgress(before, "parser.progress.select", stmtStart)
			continue
		}
		p.expect(token.Case, "parser.select", "expected case")
		comm := p.parseSimpleStmtBefore(token.Colon)
		p.expect(token.Colon, "parser.select", "expected : after select case")
		cases = append(cases, ast.CaseClause{Span: startSpan(caseStart), Comm: &comm, Body: p.parseCaseBody()})
		p.ensureProgress(before, "parser.progress.select", stmtStart)
	}
	end := p.expect(token.Rbrace, "parser.select", "expected } after select")
	return ast.Statement{Kind: ast.StmtSelect, Span: join(start, end), Cases: cases}
}

func (p *parser) parseOptionalInitAndCondition(stop token.Kind) (*ast.Statement, *ast.Expression) {
	if p.at(stop) {
		return nil, nil
	}
	first := p.parseSimpleStmtBefore(token.Semicolon, stop)
	if p.match(token.Semicolon) {
		var cond *ast.Expression
		if !p.at(stop) {
			expr := p.parseExpressionBeforeBlock()
			cond = &expr
		}
		return &first, cond
	}
	if first.Kind == ast.StmtExpr {
		return nil, first.Expr
	}
	return &first, nil
}

func (p *parser) parseSimpleStmtBefore(stops ...token.Kind) ast.Statement {
	if p.stopAt(stops...) {
		return ast.Statement{Kind: ast.StmtEmpty, Span: p.current().Span}
	}
	if containsKind(stops, token.Lbrace) {
		p.noComposite++
		defer func() { p.noComposite-- }()
	}
	return p.parseSimpleStmtWithStops(stops...)
}

func (p *parser) parseExpressionBeforeBlock() ast.Expression {
	p.noComposite++
	defer func() { p.noComposite-- }()
	return p.parseExpression(1)
}

func (p *parser) parseNestedExpression() ast.Expression {
	if p.noComposite == 0 {
		return p.parseExpression(1)
	}
	previous := p.noComposite
	p.noComposite = 0
	defer func() { p.noComposite = previous }()
	return p.parseExpression(1)
}

func (p *parser) parseSimpleStmt() ast.Statement {
	return p.parseSimpleStmtWithStops(token.Semicolon, token.Rbrace, token.EOF)
}

func (p *parser) parseSimpleStmtWithStops(stops ...token.Kind) ast.Statement {
	start := p.current()
	if p.match(token.Range) {
		rangeExpr := p.parseExpression(1)
		return ast.Statement{Kind: ast.StmtRange, Span: exprSpan(start, rangeExpr), Range: &rangeExpr}
	}
	if p.at(token.Arrow) {
		expr := p.parseExpression(1)
		return ast.Statement{Kind: ast.StmtExpr, Span: expr.Span, Expr: &expr}
	}
	left := p.parseExpressionListUntil(append(stops, token.Assign, token.Define, token.AddAssign, token.SubAssign, token.MulAssign, token.QuoAssign, token.RemAssign, token.AndAssign, token.OrAssign, token.XorAssign, token.ShlAssign, token.ShrAssign, token.AndNotAssign, token.Arrow, token.Inc, token.Dec)...)
	if len(left) == 0 {
		return ast.Statement{Kind: ast.StmtEmpty, Span: startSpan(start)}
	}
	if p.match(token.Inc) || p.match(token.Dec) {
		op := p.previous().Lexeme
		return ast.Statement{Kind: ast.StmtAssign, Span: spanJoin(startSpan(start), p.previous().Span), Left: left, Op: op}
	}
	if p.match(token.Arrow) {
		value := p.parseExpression(1)
		return ast.Statement{Kind: ast.StmtSend, Span: exprSpan(start, value), Left: left, Right: []ast.Expression{value}}
	}
	if p.atAssignmentOp() {
		op := p.advance().Lexeme
		if p.at(token.Range) {
			p.advance()
			rangeExpr := p.parseExpression(1)
			stmt := ast.Statement{Kind: ast.StmtRange, Span: exprSpan(start, rangeExpr), Op: op, Range: &rangeExpr}
			if len(left) > 0 {
				stmt.Key = &left[0]
			}
			if len(left) > 1 {
				stmt.Value = &left[1]
			}
			return stmt
		}
		right := p.parseExpressionListUntil(stops...)
		return ast.Statement{Kind: ast.StmtAssign, Span: spanJoin(startSpan(start), lastExprSpan(right, left)), Left: left, Right: right, Op: op}
	}
	if len(left) == 1 {
		if left[0].Kind == ast.ExprInvalid {
			return ast.Statement{Kind: ast.StmtInvalid, Span: left[0].Span}
		}
		if panicExpr, ok := panicCall(left[0]); ok {
			return ast.Statement{Kind: ast.StmtPanic, Span: left[0].Span, Expr: panicExpr}
		}
		return ast.Statement{Kind: ast.StmtExpr, Span: left[0].Span, Expr: &left[0]}
	}
	p.add("parser.stmt.expr.multi", "expression statement must contain a single expression", spanJoin(left[0].Span, left[len(left)-1].Span))
	return ast.Statement{Kind: ast.StmtExpr, Span: spanJoin(left[0].Span, left[len(left)-1].Span), Expr: &left[0]}
}

func (p *parser) parseExpressionListUntil(stops ...token.Kind) []ast.Expression {
	if p.stopAt(stops...) {
		return nil
	}
	var out []ast.Expression
	for {
		before := p.pos
		out = append(out, p.parseExpression(1))
		if !p.match(token.Comma) {
			break
		}
		if p.stopAt(stops...) {
			break
		}
		p.ensureProgress(before, "parser.progress.expr_list", exprEnd)
	}
	return out
}
