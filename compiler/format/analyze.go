package format

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

type braceRole uint8

const (
	braceUnknown braceRole = iota
	braceBlock
	braceType
	braceComposite
)

type operatorRole uint8

const (
	operatorDefault operatorRole = iota
	operatorBinary
	operatorPrefix
	operatorChanSend
	operatorPostfix
)

type colonRole uint8

const (
	colonDefault colonRole = iota
	colonTight
	colonClause
	colonLabel
)

type syntaxFacts struct {
	braces          map[int]braceRole
	operators       map[int]operatorRole
	clauseSemicolon map[int]struct{}
	colons          map[int]colonRole
	caseOutdent     map[int]struct{}
	groupParens     map[int]bool
	blankBefore     map[int]struct{}
	spaceBefore     map[int]struct{}
	groupedDecls    [][2]int
}

type factCollector struct {
	document parser.Syntax
	facts    syntaxFacts
}

func analyzeSyntax(document parser.Syntax) syntaxFacts {
	collector := factCollector{document: document, facts: syntaxFacts{
		braces:          map[int]braceRole{},
		operators:       map[int]operatorRole{},
		clauseSemicolon: map[int]struct{}{},
		colons:          map[int]colonRole{},
		caseOutdent:     map[int]struct{}{},
		groupParens:     map[int]bool{},
		blankBefore:     map[int]struct{}{},
		spaceBefore:     map[int]struct{}{},
	}}
	collector.groupedDeclarations()
	for fileIndex := range document.Program.Files {
		file := &document.Program.Files[fileIndex]
		previousStart := -1
		previousEnd := document.Program.PackageID.Span.End.Offset
		previousKind := ast.DeclInvalid
		for declIndex := range file.Decls {
			decl := &file.Decls[declIndex]
			if decl.Span.Valid() && decl.Span.Start.Offset != previousStart {
				if previousStart < 0 || decl.Kind != ast.DeclImport || previousKind != ast.DeclImport {
					collector.markDeclarationBreak(decl.Span.Start.Offset, previousEnd)
				}
				previousStart = decl.Span.Start.Offset
				previousEnd = decl.Span.End.Offset
				if groupedEnd, ok := collector.groupedDeclarationEnd(previousStart); ok {
					previousEnd = groupedEnd
				}
				previousKind = decl.Kind
			}
			collector.decl(decl)
		}
	}
	return collector.facts
}

func (c *factCollector) decl(decl *ast.Decl) {
	if decl == nil {
		return
	}
	switch decl.Kind {
	case ast.DeclConst:
		c.valueDecl(decl.Const)
	case ast.DeclVar:
		c.valueDecl(decl.Var)
	case ast.DeclType:
		for index := range decl.Type.TypeParams {
			c.typ(&decl.Type.TypeParams[index].Constraint)
		}
		c.typ(&decl.Type.Type)
	case ast.DeclFunc:
		c.function(decl.Func)
	}
}

func (c *factCollector) valueDecl(decl *ast.ValueDecl) {
	if decl == nil {
		return
	}
	c.typ(&decl.Type)
	for index := range decl.Values {
		c.expr(&decl.Values[index])
	}
}

func (c *factCollector) function(function *ast.FuncDecl) {
	if function == nil {
		return
	}
	if function.Receiver != nil {
		c.field(function.Receiver)
	}
	for index := range function.TypeParams {
		c.typ(&function.TypeParams[index].Constraint)
	}
	for index := range function.Params {
		c.field(&function.Params[index])
	}
	for index := range function.Results {
		c.field(&function.Results[index])
	}
	c.block(&function.Body)
}

func (c *factCollector) block(block *ast.BlockStmt) {
	if block == nil {
		return
	}
	if block.Span.Valid() {
		c.markBrace(block.Span, braceBlock, block.Span.Start.Offset)
	}
	for index := range block.Stmts {
		c.stmt(&block.Stmts[index])
	}
}

func (c *factCollector) stmt(stmt *ast.Statement) {
	if stmt == nil {
		return
	}
	for index := range stmt.Decls {
		c.decl(&stmt.Decls[index])
	}
	c.expr(stmt.Expr)
	for _, expressions := range [][]ast.Expression{stmt.Left, stmt.Right, stmt.Results} {
		for index := range expressions {
			c.expr(&expressions[index])
		}
	}
	c.expr(stmt.Cond)
	c.expr(stmt.Range)
	if stmt.Kind == ast.StmtAssign && strings.TrimSpace(stmt.Op) != "" {
		start, end := stmt.Span.Start.Offset, stmt.Span.End.Offset
		if len(stmt.Left) != 0 {
			start = stmt.Left[len(stmt.Left)-1].Span.End.Offset
		}
		if len(stmt.Right) != 0 {
			end = stmt.Right[0].Span.Start.Offset
		}
		role := operatorBinary
		if stmt.Op == "++" || stmt.Op == "--" {
			role = operatorPostfix
		}
		c.markOperatorBetween(start, end, stmt.Op, role)
	}
	if stmt.Kind == ast.StmtSend {
		start, end := stmt.Span.Start.Offset, stmt.Span.End.Offset
		if len(stmt.Left) != 0 {
			start = stmt.Left[len(stmt.Left)-1].Span.End.Offset
		}
		if len(stmt.Right) != 0 {
			end = stmt.Right[0].Span.Start.Offset
		}
		c.markOperatorBetween(start, end, "<-", operatorBinary)
	}
	caseOpen, caseClose := -1, -1
	if stmt.Kind == ast.StmtSwitch || stmt.Kind == ast.StmtSelect {
		caseOpen, caseClose = c.markBrace(stmt.Span, braceBlock, stmt.Span.Start.Offset)
	}
	if stmt.Kind == ast.StmtFor || stmt.Kind == ast.StmtIf || stmt.Kind == ast.StmtSwitch {
		end := stmt.Span.End.Offset
		if stmt.Body.Span.Valid() {
			end = stmt.Body.Span.Start.Offset
		} else if caseOpen >= 0 {
			end = caseOpen
		}
		for _, candidate := range c.document.Tokens {
			if candidate.Kind == token.Semicolon && candidate.Lexeme == ";" && candidate.Span.Start.Offset >= stmt.Span.Start.Offset && candidate.Span.End.Offset <= end {
				c.facts.clauseSemicolon[candidate.Span.Start.Offset] = struct{}{}
			}
		}
	}
	if stmt.Kind == ast.StmtLabel && stmt.Span.Valid() {
		c.markTokenBetween(stmt.Span.Start.Offset, stmt.Span.End.Offset, token.Colon, colonLabel)
	}
	c.stmt(stmt.Init)
	c.stmt(stmt.Post)
	c.stmt(stmt.Else)
	c.block(&stmt.Body)
	for index := range stmt.Cases {
		clause := &stmt.Cases[index]
		for valueIndex := range clause.Values {
			c.expr(&clause.Values[valueIndex])
		}
		for typeIndex := range clause.Types {
			c.typ(&clause.Types[typeIndex])
		}
		c.stmt(clause.Comm)
		colonStart := clause.Span.End.Offset
		if len(clause.Values) != 0 {
			colonStart = clause.Values[len(clause.Values)-1].Span.End.Offset
		}
		if len(clause.Types) != 0 {
			colonStart = clause.Types[len(clause.Types)-1].Span.End.Offset
		}
		if clause.Comm != nil {
			colonStart = clause.Comm.Span.End.Offset
		}
		boundary := caseClose
		if index+1 < len(stmt.Cases) {
			boundary = stmt.Cases[index+1].Span.Start.Offset
		}
		if boundary >= 0 {
			c.markTokenBetween(colonStart, boundary, token.Colon, colonClause)
			c.facts.caseOutdent[boundary] = struct{}{}
		}
		c.block(&clause.Body)
	}
}

func (c *factCollector) expr(expr *ast.Expression) {
	if expr == nil {
		return
	}
	c.typ(expr.Type)
	switch expr.Kind {
	case ast.ExprUnary, ast.ExprAddr, ast.ExprDeref, ast.ExprReceive:
		c.markFirstOperator(expr.Span, expr.Operator, operatorPrefix)
	case ast.ExprBinary:
		start, end := expr.Span.Start.Offset, expr.Span.End.Offset
		if expr.Left != nil {
			start = expr.Left.Span.End.Offset
		}
		if expr.Right != nil {
			end = expr.Right.Span.Start.Offset
		}
		c.markOperatorBetween(start, end, expr.Operator, operatorBinary)
	case ast.ExprSlice:
		for _, candidate := range c.document.Tokens {
			if candidate.Kind == token.Colon && candidate.Span.Start.Offset >= expr.Span.Start.Offset && candidate.Span.End.Offset <= expr.Span.End.Offset {
				c.facts.colons[candidate.Span.Start.Offset] = colonTight
			}
		}
	case ast.ExprComposite:
		start := expr.Span.Start.Offset
		if expr.Type != nil && expr.Type.Span.Valid() {
			start = expr.Type.Span.End.Offset
		}
		c.markBrace(expr.Span, braceComposite, start)
		for index := range expr.Items {
			item := &expr.Items[index]
			c.expr(item.Key)
			c.expr(&item.Value)
			if item.Key != nil {
				c.markTokenBetween(item.Key.Span.End.Offset, item.Value.Span.Start.Offset, token.Colon, colonDefault)
			}
		}
	case ast.ExprFunc:
		c.function(expr.Func)
	}
	c.expr(expr.Left)
	c.expr(expr.Right)
	c.expr(expr.Operand)
	c.expr(expr.Callee)
	for index := range expr.Args {
		c.expr(&expr.Args[index])
	}
	c.expr(expr.Index)
	c.expr(expr.Start)
	c.expr(expr.End)
	c.expr(expr.Max)
}

func (c *factCollector) typ(typ *ast.TypeExpr) {
	if typ == nil || typ.Kind == ast.TypeInvalid {
		return
	}
	switch typ.Kind {
	case ast.TypePointer:
		c.markFirstOperator(typ.Span, "*", operatorPrefix)
	case ast.TypeChan:
		switch typ.Direction {
		case "recv":
			c.markFirstOperator(typ.Span, "<-", operatorPrefix)
		case "send":
			c.markFirstOperator(typ.Span, "<-", operatorChanSend)
		}
	case ast.TypeStruct, ast.TypeInterface:
		c.markBrace(typ.Span, braceType, typ.Span.Start.Offset)
	}
	c.typ(typ.Base)
	c.typ(typ.Elem)
	c.typ(typ.Key)
	c.expr(typ.Len)
	for _, fields := range [][]ast.Field{typ.Params, typ.Results, typ.Fields} {
		for index := range fields {
			c.field(&fields[index])
		}
	}
	for index := range typ.Methods {
		c.function(&typ.Methods[index])
	}
	for index := range typ.Embeds {
		c.typ(&typ.Embeds[index])
	}
	for index := range typ.Terms {
		if typ.Terms[index].Approx {
			c.markFirstOperator(typ.Terms[index].Span, "~", operatorPrefix)
		}
		c.typ(&typ.Terms[index].Type)
	}
	for index := range typ.TypeArgs {
		c.typ(&typ.TypeArgs[index])
	}
}

func (c *factCollector) field(field *ast.Field) {
	if field == nil {
		return
	}
	if field.Variadic {
		for _, candidate := range c.document.Tokens {
			if candidate.Kind == token.Ellipsis && candidate.Span.Start.Offset >= field.Span.Start.Offset && candidate.Span.End.Offset <= field.Span.End.Offset {
				c.facts.spaceBefore[candidate.Span.Start.Offset] = struct{}{}
				break
			}
		}
	}
	c.typ(&field.Type)
}

func (c *factCollector) markBrace(span source.Span, role braceRole, searchStart int) (int, int) {
	if !span.Valid() {
		return -1, -1
	}
	open := -1
	depth := 0
	for index, candidate := range c.document.Tokens {
		if candidate.Span.Start.Offset < searchStart || candidate.Span.End.Offset > span.End.Offset {
			continue
		}
		if candidate.Kind == token.Lbrace {
			if open < 0 {
				open = index
			}
			depth++
			continue
		}
		if candidate.Kind == token.Rbrace && open >= 0 {
			depth--
			if depth == 0 {
				c.facts.braces[c.document.Tokens[open].Span.Start.Offset] = role
				c.facts.braces[candidate.Span.Start.Offset] = role
				return c.document.Tokens[open].Span.Start.Offset, candidate.Span.Start.Offset
			}
		}
	}
	return -1, -1
}

func (c *factCollector) groupedDeclarations() {
	for index, candidate := range c.document.Tokens {
		switch candidate.Kind {
		case token.Import, token.Const, token.Var, token.Type:
		default:
			continue
		}
		next := index + 1
		for next < len(c.document.Tokens) && c.document.Tokens[next].Kind == token.Semicolon {
			next++
		}
		if next >= len(c.document.Tokens) || c.document.Tokens[next].Kind != token.Lparen {
			continue
		}
		depth := 0
		for cursor := next; cursor < len(c.document.Tokens); cursor++ {
			switch c.document.Tokens[cursor].Kind {
			case token.Lparen:
				depth++
			case token.Rparen:
				depth--
				if depth == 0 {
					c.facts.groupParens[c.document.Tokens[next].Span.Start.Offset] = true
					c.facts.groupParens[c.document.Tokens[cursor].Span.Start.Offset] = false
					c.facts.groupedDecls = append(c.facts.groupedDecls, [2]int{
						candidate.Span.Start.Offset,
						c.document.Tokens[cursor].Span.End.Offset,
					})
					cursor = len(c.document.Tokens)
				}
			}
		}
	}
}

func (c *factCollector) groupedDeclarationEnd(offset int) (int, bool) {
	for _, bounds := range c.facts.groupedDecls {
		if offset == bounds[0] {
			return bounds[1], true
		}
	}
	return 0, false
}

func (c *factCollector) markDeclarationBreak(offset, previousEnd int) {
	anchor := offset
	previousLine := 0
	if position, ok := c.document.File.Position(previousEnd); ok {
		previousLine = position.Line
	}
	for _, element := range c.document.Elements {
		if element.Span.Start.Offset < previousEnd || element.Span.End.Offset > offset {
			continue
		}
		if (element.Kind == scanner.ElementLineComment || element.Kind == scanner.ElementBlockComment) && element.Span.Start.Line > previousLine {
			anchor = element.Span.Start.Offset
			break
		}
	}
	c.facts.blankBefore[anchor] = struct{}{}
}

func (c *factCollector) markFirstOperator(span source.Span, lexeme string, role operatorRole) {
	if lexeme == "" || !span.Valid() {
		return
	}
	for _, candidate := range c.document.Tokens {
		if candidate.Span.Start.Offset < span.Start.Offset || candidate.Span.End.Offset > span.End.Offset {
			continue
		}
		if candidate.Lexeme == lexeme {
			c.facts.operators[candidate.Span.Start.Offset] = role
			return
		}
	}
}

func (c *factCollector) markOperatorBetween(start, end int, lexeme string, role operatorRole) {
	lexeme = strings.TrimSpace(lexeme)
	for _, candidate := range c.document.Tokens {
		if candidate.Span.Start.Offset >= start && candidate.Span.End.Offset <= end && candidate.Lexeme == lexeme {
			c.facts.operators[candidate.Span.Start.Offset] = role
			return
		}
	}
}

func (c *factCollector) markTokenBetween(start, end int, kind token.Kind, role colonRole) {
	for _, candidate := range c.document.Tokens {
		if candidate.Kind == kind && candidate.Span.Start.Offset >= start && candidate.Span.End.Offset <= end {
			c.facts.colons[candidate.Span.Start.Offset] = role
			return
		}
	}
}
