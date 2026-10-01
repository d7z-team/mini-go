package parser

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

func (p *parser) parseExpression(minPrecedence int) ast.Expression {
	if !p.enterNest("parser.expr.depth", "expression nesting too deep") {
		return ast.Expression{Kind: ast.ExprInvalid, Span: p.current().Span}
	}
	defer p.leaveNest()
	left := p.parseUnary()
	for {
		precedence := binaryPrecedence(p.current().Kind)
		if precedence < minPrecedence {
			break
		}
		op := p.advance()
		right := p.parseExpression(precedence + 1)
		leftNode := left
		rightNode := right
		left = ast.Expression{Kind: ast.ExprBinary, Span: spanJoin(left.Span, right.Span), Operator: op.Lexeme, Left: &leftNode, Right: &rightNode}
	}
	return left
}

func (p *parser) parseUnary() ast.Expression {
	start := p.current()
	switch start.Kind {
	case token.Add, token.Sub, token.Not, token.Xor, token.Mul, token.And, token.Arrow:
		p.advance()
		operand := p.parseUnary()
		kind := ast.ExprUnary
		switch start.Kind {
		case token.And:
			kind = ast.ExprAddr
		case token.Mul:
			kind = ast.ExprDeref
		case token.Arrow:
			kind = ast.ExprReceive
		}
		return ast.Expression{Kind: kind, Span: spanJoin(startSpan(start), operand.Span), Operator: start.Lexeme, Operand: &operand}
	default:
		return p.parsePostfix()
	}
}

func (p *parser) parsePostfix() ast.Expression {
	expr := p.parsePrimary()
	for {
		switch p.current().Kind {
		case token.Period:
			p.advance()
			if p.match(token.Lparen) {
				if p.match(token.Type) {
					p.expect(token.Rparen, "parser.assert", "expected ) after type")
					operand := expr
					expr = ast.Expression{Kind: ast.ExprAssert, Span: spanJoin(expr.Span, p.previous().Span), Operand: &operand, TypeSwitch: true}
					continue
				}
				typ := p.parseType()
				p.expect(token.Rparen, "parser.assert", "expected ) after assertion type")
				operand := expr
				expr = ast.Expression{Kind: ast.ExprAssert, Span: spanJoin(expr.Span, p.previous().Span), Operand: &operand, Type: &typ}
				continue
			}
			field := p.expect(token.Ident, "parser.selector", "expected selector")
			operand := expr
			expr = ast.Expression{Kind: ast.ExprSelector, Span: spanJoin(expr.Span, field.Span), Operand: &operand, Field: field.Lexeme, NameID: identifier(field)}
		case token.Lparen:
			expr = p.finishCall(expr)
		case token.Lbrack:
			expr = p.finishIndexOrSlice(expr)
		case token.Lbrace:
			if p.noComposite == 0 {
				if typ, ok := exprAsType(expr); ok {
					expr = p.finishComposite(typ, expr.Span)
				} else {
					return expr
				}
			} else {
				return expr
			}
		default:
			return expr
		}
	}
}

func (p *parser) parsePrimary() ast.Expression {
	start := p.current()
	switch start.Kind {
	case token.Ident:
		p.advance()
		if start.Lexeme == "true" || start.Lexeme == "false" {
			return ast.Expression{Kind: ast.ExprLiteral, Span: start.Span, Literal: start.Lexeme, Type: &ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}}
		}
		if start.Lexeme == "nil" {
			return ast.Expression{Kind: ast.ExprLiteral, Span: start.Span, Literal: "nil", Type: &ast.TypeExpr{Kind: ast.TypeName, Name: "Any"}}
		}
		return ast.Expression{Kind: ast.ExprIdent, Span: start.Span, Name: start.Lexeme, NameID: identifier(start)}
	case token.Int, token.Float, token.Imag, token.Char, token.String:
		p.advance()
		return p.literalExpression(start)
	case token.Lparen:
		if p.looksLikeParenthesizedTypePrimary() {
			typ := p.parseType()
			if p.at(token.Lbrace) {
				return p.finishComposite(typ, typ.Span)
			}
			return ast.Expression{Kind: ast.ExprIdent, Span: typ.Span, Name: "type", Type: &typ}
		}
		p.advance()
		expr := p.parseNestedExpression()
		end := p.expect(token.Rparen, "parser.group", "expected )")
		expr.Span = spanJoin(start.Span, end.Span)
		return expr
	case token.Func:
		probe := *p
		probe.diagnostics = nil
		if typ := probe.parseType(); typ.Kind != ast.TypeInvalid && len(probe.diagnostics) == 0 && probe.at(token.Lbrace) {
			return p.parseFuncLiteral()
		}
		typ := p.parseType()
		return ast.Expression{Kind: ast.ExprIdent, Span: typ.Span, Name: "type", Type: &typ}
	case token.Lbrack, token.Map, token.Struct, token.Interface, token.Chan:
		typ := p.parseType()
		if p.at(token.Lbrace) {
			return p.finishComposite(typ, typ.Span)
		}
		return ast.Expression{Kind: ast.ExprIdent, Span: typ.Span, Name: "type", Type: &typ}
	case token.Lbrace:
		return p.finishComposite(ast.TypeExpr{Kind: ast.TypeInvalid, Span: startSpan(start)}, startSpan(start))
	default:
		p.errorAtCurrent("parser.expr", "expected expression")
		p.advance()
		return ast.Expression{Kind: ast.ExprInvalid, Span: startSpan(start)}
	}
}

func (p *parser) parseFuncLiteral() ast.Expression {
	start := p.expect(token.Func, "parser.func_lit", "expected func")
	params, results := p.parseSignature()
	body := p.parseBlock()
	decl := ast.FuncDecl{Params: params, Results: results, Body: body}
	return ast.Expression{Kind: ast.ExprFunc, Span: spanJoin(startSpan(start), body.Span), Func: &decl}
}

func (p *parser) finishCall(callee ast.Expression) ast.Expression {
	p.expect(token.Lparen, "parser.call", "expected (")
	var args []ast.Expression
	ellipsis := false
	for !p.at(token.Rparen) && !p.at(token.EOF) {
		before := p.pos
		if p.match(token.Ellipsis) {
			ellipsis = true
			break
		}
		if len(args) == 0 && p.shouldParseTypeArgBuiltin(callee) {
			typ := p.parseType()
			args = append(args, ast.Expression{Kind: ast.ExprIdent, Span: typ.Span, Name: "type", Type: &typ})
		} else {
			args = append(args, p.parseNestedExpression())
		}
		if p.match(token.Ellipsis) {
			ellipsis = true
		}
		if !p.match(token.Comma) {
			break
		}
		p.ensureProgress(before, "parser.progress.call", exprEnd)
	}
	end := p.expect(token.Rparen, "parser.call", "expected )")
	if typ, ok := calleeType(callee); ok {
		if ellipsis {
			p.add("parser.convert.ellipsis", "type conversion cannot use ellipsis", spanJoin(callee.Span, end.Span))
		}
		if len(args) != 1 {
			p.add("parser.convert.arg_count", "type conversion requires exactly one argument", spanJoin(callee.Span, end.Span))
			return ast.Expression{Kind: ast.ExprCall, Span: spanJoin(callee.Span, end.Span), Callee: &callee, Args: args, Ellipsis: ellipsis}
		}
		operand := args[0]
		return ast.Expression{Kind: ast.ExprConvert, Span: spanJoin(callee.Span, end.Span), Type: &typ, Operand: &operand}
	}
	return ast.Expression{Kind: ast.ExprCall, Span: spanJoin(callee.Span, end.Span), Callee: &callee, Args: args, Ellipsis: ellipsis}
}

func (p *parser) finishIndexOrSlice(receiver ast.Expression) ast.Expression {
	p.expect(token.Lbrack, "parser.index", "expected [")
	var first *ast.Expression
	if !p.at(token.Colon) {
		expr := p.parseNestedExpression()
		first = &expr
	}
	if p.match(token.Colon) {
		var endExpr *ast.Expression
		var maxExpr *ast.Expression
		if !p.at(token.Colon) && !p.at(token.Rbrack) {
			expr := p.parseNestedExpression()
			endExpr = &expr
		}
		if p.match(token.Colon) && !p.at(token.Rbrack) {
			expr := p.parseNestedExpression()
			maxExpr = &expr
		}
		end := p.expect(token.Rbrack, "parser.slice", "expected ]")
		return ast.Expression{Kind: ast.ExprSlice, Span: spanJoin(receiver.Span, end.Span), Operand: &receiver, Start: first, End: endExpr, Max: maxExpr}
	}
	if p.match(token.Comma) {
		args := []ast.Expression{*first}
		for !p.at(token.Rbrack) && !p.at(token.EOF) {
			args = append(args, p.parseNestedExpression())
			if !p.match(token.Comma) {
				break
			}
		}
		end := p.expect(token.Rbrack, "parser.index_list", "expected ]")
		return ast.Expression{Kind: ast.ExprIndexList, Span: spanJoin(receiver.Span, end.Span), Operand: &receiver, Args: args}
	}
	end := p.expect(token.Rbrack, "parser.index", "expected ]")
	return ast.Expression{Kind: ast.ExprIndex, Span: spanJoin(receiver.Span, end.Span), Operand: &receiver, Index: first}
}

func (p *parser) finishComposite(typ ast.TypeExpr, startSpan source.Span) ast.Expression {
	p.expect(token.Lbrace, "parser.composite", "expected {")
	var items []ast.KeyValue
	for !p.at(token.Rbrace) && !p.at(token.EOF) {
		before := p.pos
		if p.at(token.Comma) {
			p.advance()
			continue
		}
		first := p.parseNestedExpression()
		if p.match(token.Colon) {
			value := p.parseNestedExpression()
			entry := ast.KeyValue{Key: &first, Value: value}
			items = append(items, entry)
		} else {
			items = append(items, ast.KeyValue{Value: first})
		}
		if !p.match(token.Comma) {
			break
		}
		p.ensureProgress(before, "parser.progress.composite", exprEnd)
	}
	end := p.expect(token.Rbrace, "parser.composite", "expected }")
	var explicitType *ast.TypeExpr
	if typ.Kind != ast.TypeInvalid {
		explicitType = &typ
	}
	return ast.Expression{Kind: ast.ExprComposite, Span: spanJoin(startSpan, end.Span), Type: explicitType, Items: items}
}

func (p *parser) parseIdentList() ([]string, []ast.Identifier) {
	var names []string
	var identifiers []ast.Identifier
	first := p.expectIdentToken("parser.ident", "expected identifier")
	names = append(names, first.Lexeme)
	identifiers = append(identifiers, identifier(first))
	for p.match(token.Comma) {
		value := p.expectIdentToken("parser.ident", "expected identifier")
		names = append(names, value.Lexeme)
		identifiers = append(identifiers, identifier(value))
	}
	return names, identifiers
}

func (p *parser) canStartType() bool {
	switch p.current().Kind {
	case token.Ident, token.Lparen, token.Mul, token.Lbrack, token.Map, token.Chan, token.Arrow, token.Func, token.Struct, token.Interface:
		return true
	default:
		return false
	}
}

func (p *parser) looksLikeParenthesizedTypePrimary() bool {
	if !p.at(token.Lparen) {
		return false
	}
	pos := p.pos + 1
	for pos < p.lexical.TokenCount() && p.lexical.TokenKind(pos) == token.Lparen {
		pos++
	}
	if pos >= p.lexical.TokenCount() || !isTypeOnlyPrimaryStart(p.lexical.TokenKind(pos)) {
		return false
	}
	probe := *p
	probe.diagnostics = nil
	typ := probe.parseType()
	return typ.Kind != ast.TypeInvalid && len(probe.diagnostics) == 0 && (probe.at(token.Lparen) || probe.at(token.Lbrace))
}

func (p *parser) shouldParseTypeArgBuiltin(callee ast.Expression) bool {
	if callee.Kind != ast.ExprIdent {
		return false
	}
	switch callee.Name {
	case "make", "new":
		if isTypeOnlyPrimaryStart(p.current().Kind) {
			return true
		}
		return p.at(token.Lparen) && p.parenthesizedNewArgumentStartsWithTypeSyntax()
	default:
		return false
	}
}

func (p *parser) parenthesizedNewArgumentStartsWithTypeSyntax() bool {
	pos := p.pos
	for pos < p.lexical.TokenCount() && p.lexical.TokenKind(pos) == token.Lparen {
		pos++
	}
	if pos >= p.lexical.TokenCount() {
		return false
	}
	tok := p.lexical.TokenRecord(pos)
	if isTypeOnlyPrimaryStart(tok.Kind) {
		return true
	}
	return tok.Kind == token.Ident && isPredeclaredTypeName(tok.Lexeme)
}

func isTypeOnlyPrimaryStart(kind token.Kind) bool {
	switch kind {
	case token.Mul, token.Lbrack, token.Map, token.Chan, token.Arrow, token.Func, token.Struct, token.Interface:
		return true
	default:
		return false
	}
}

func (p *parser) looksLikeReceiver() bool {
	depth := 0
	for i := p.pos; i < p.lexical.TokenCount(); i++ {
		switch p.lexical.TokenKind(i) {
		case token.Lparen:
			depth++
		case token.Rparen:
			depth--
			if depth == 0 {
				return i+1 < p.lexical.TokenCount() && p.lexical.TokenKind(i+1) == token.Ident
			}
		}
	}
	return false
}

func (p *parser) atAssignmentOp() bool {
	switch p.current().Kind {
	case token.Assign, token.Define, token.AddAssign, token.SubAssign, token.MulAssign, token.QuoAssign,
		token.RemAssign, token.AndAssign, token.OrAssign, token.XorAssign, token.ShlAssign, token.ShrAssign, token.AndNotAssign:
		return true
	default:
		return false
	}
}

func binaryPrecedence(kind token.Kind) int {
	switch kind {
	case token.Lor:
		return 1
	case token.Land:
		return 2
	case token.Eq, token.Ne, token.Lt, token.Le, token.Gt, token.Ge:
		return 3
	case token.Add, token.Sub, token.Or, token.Xor:
		return 4
	case token.Mul, token.Quo, token.Rem, token.Shl, token.Shr, token.And, token.AndNot:
		return 5
	default:
		return 0
	}
}
