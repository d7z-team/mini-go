package parser

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/token"
)

func (p *parser) literalExpression(scanned scanner.Token) ast.Expression {
	text := strings.ReplaceAll(scanned.Lexeme, "_", "")
	typ := literalType(scanned)
	switch scanned.Kind {
	case token.Int, token.Float, token.Imag:
		// Numeric source spelling remains exact until compiler constant evaluation.
		return ast.Expression{Kind: ast.ExprLiteral, Span: scanned.Span, Literal: text, Type: &typ}
	case token.String:
		value, err := strconv.Unquote(scanned.Lexeme)
		if err != nil {
			p.add("parser.literal.string", "invalid string literal", scanned.Span)
			return ast.Expression{Kind: ast.ExprInvalid, Span: scanned.Span}
		}
		return ast.Expression{Kind: ast.ExprLiteral, Span: scanned.Span, Literal: strconv.Quote(value), Type: &typ}
	case token.Char:
		literal := scanned.Lexeme
		if len(literal) < 2 {
			p.add("parser.literal.char", "invalid char literal", scanned.Span)
			return ast.Expression{Kind: ast.ExprInvalid, Span: scanned.Span}
		}
		value, _, tail, err := strconv.UnquoteChar(literal[1:len(literal)-1], '\'')
		if err != nil || tail != "" {
			p.add("parser.literal.char", "invalid char literal", scanned.Span)
			return ast.Expression{Kind: ast.ExprInvalid, Span: scanned.Span}
		}
		return ast.Expression{Kind: ast.ExprLiteral, Span: scanned.Span, Literal: strconv.FormatInt(int64(value), 10), Type: &typ}
	default:
		return ast.Expression{Kind: ast.ExprLiteral, Span: scanned.Span, Literal: scanned.Lexeme, Type: &typ}
	}
}

func literalType(scanned scanner.Token) ast.TypeExpr {
	switch scanned.Kind {
	case token.String:
		return ast.TypeExpr{Kind: ast.TypeName, Name: "String", Span: scanned.Span}
	case token.Int:
		return ast.TypeExpr{Kind: ast.TypeName, Name: "Int", Span: scanned.Span}
	case token.Float:
		return ast.TypeExpr{Kind: ast.TypeName, Name: "Float64", Span: scanned.Span}
	case token.Imag:
		return ast.TypeExpr{Kind: ast.TypeName, Name: "Complex128", Span: scanned.Span}
	case token.Char:
		return ast.TypeExpr{Kind: ast.TypeName, Name: "Int32", Span: scanned.Span}
	default:
		return ast.TypeExpr{Kind: ast.TypeName, Name: "Any", Span: scanned.Span}
	}
}

func replaceIotaExpressions(expressions []ast.Expression, value int) []ast.Expression {
	out := make([]ast.Expression, len(expressions))
	for i := range expressions {
		out[i] = ast.CloneExpression(expressions[i])
		replaceIotaExpression(&out[i], value)
	}
	return out
}

func replaceIotaExpression(expr *ast.Expression, value int) {
	if expr == nil {
		return
	}
	if expr.Kind == ast.ExprIdent && expr.Name == "iota" {
		*expr = ast.Expression{
			Kind:    ast.ExprLiteral,
			Span:    expr.Span,
			Literal: strconv.Itoa(value),
			Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: "Int", Span: expr.Span},
		}
		return
	}
	replaceIotaExpression(expr.Left, value)
	replaceIotaExpression(expr.Right, value)
	replaceIotaExpression(expr.Operand, value)
	replaceIotaExpression(expr.Callee, value)
	for i := range expr.Args {
		replaceIotaExpression(&expr.Args[i], value)
	}
	replaceIotaExpression(expr.Index, value)
	replaceIotaExpression(expr.Start, value)
	replaceIotaExpression(expr.End, value)
	replaceIotaExpression(expr.Max, value)
	for i := range expr.Items {
		replaceIotaExpression(expr.Items[i].Key, value)
		replaceIotaExpression(&expr.Items[i].Value, value)
	}
	if expr.Func != nil {
		for i := range expr.Func.Body.Stmts {
			replaceIotaStatement(&expr.Func.Body.Stmts[i], value)
		}
	}
}

func replaceIotaStatement(stmt *ast.Statement, value int) {
	if stmt == nil {
		return
	}
	replaceIotaExpression(stmt.Expr, value)
	for i := range stmt.Left {
		replaceIotaExpression(&stmt.Left[i], value)
	}
	for i := range stmt.Right {
		replaceIotaExpression(&stmt.Right[i], value)
	}
	replaceIotaStatement(stmt.Init, value)
	replaceIotaExpression(stmt.Cond, value)
	replaceIotaStatement(stmt.Post, value)
	replaceIotaStatement(stmt.Else, value)
	replaceIotaExpression(stmt.Range, value)
	for i := range stmt.Body.Stmts {
		replaceIotaStatement(&stmt.Body.Stmts[i], value)
	}
	for i := range stmt.Cases {
		clause := &stmt.Cases[i]
		for j := range clause.Values {
			replaceIotaExpression(&clause.Values[j], value)
		}
		replaceIotaStatement(clause.Comm, value)
		for j := range clause.Body.Stmts {
			replaceIotaStatement(&clause.Body.Stmts[j], value)
		}
	}
	for i := range stmt.Results {
		replaceIotaExpression(&stmt.Results[i], value)
	}
}

func calleeType(callee ast.Expression) (ast.TypeExpr, bool) {
	if callee.Kind == ast.ExprIdent && callee.Name == "type" && callee.Type != nil && callee.Type.Kind != ast.TypeInvalid {
		return *callee.Type, true
	}
	return ast.TypeExpr{}, false
}

func isPredeclaredTypeName(name string) bool {
	switch name {
	case "bool", "string",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"byte", "rune",
		"float32", "float64",
		"complex64", "complex128",
		"any", "error":
		return true
	default:
		return false
	}
}

func exprAsType(expr ast.Expression) (ast.TypeExpr, bool) {
	switch expr.Kind {
	case ast.ExprIdent:
		if expr.Name == "type" && expr.Type != nil && expr.Type.Kind != ast.TypeInvalid {
			return *expr.Type, true
		}
		return ast.TypeExpr{Kind: ast.TypeName, Name: expr.Name, Span: expr.Span}, true
	case ast.ExprSelector:
		if expr.Operand != nil && expr.Operand.Kind == ast.ExprIdent {
			name := expr.Operand.Name + "." + expr.Field
			return ast.TypeExpr{Kind: ast.TypeName, Name: name, Span: expr.Span}, true
		}
	case ast.ExprIndex, ast.ExprIndexList:
		if expr.Operand == nil {
			return ast.TypeExpr{}, false
		}
		base, ok := exprAsType(*expr.Operand)
		if !ok {
			return ast.TypeExpr{}, false
		}
		var expressions []ast.Expression
		if expr.Kind == ast.ExprIndex && expr.Index != nil {
			expressions = []ast.Expression{*expr.Index}
		} else {
			expressions = expr.Args
		}
		args := make([]ast.TypeExpr, 0, len(expressions))
		for _, expression := range expressions {
			arg, ok := exprAsType(expression)
			if !ok {
				return ast.TypeExpr{}, false
			}
			args = append(args, arg)
		}
		if len(args) == 0 {
			return ast.TypeExpr{}, false
		}
		return ast.TypeExpr{Kind: ast.TypeInstance, Base: &base, TypeArgs: args, Span: expr.Span}, true
	}
	return ast.TypeExpr{}, false
}

func panicCall(expr ast.Expression) (*ast.Expression, bool) {
	if expr.Kind != ast.ExprCall || expr.Callee == nil || len(expr.Args) != 1 {
		return nil, false
	}
	if expr.Callee.Kind != ast.ExprIdent || expr.Callee.Name != "panic" {
		return nil, false
	}
	arg := expr.Args[0]
	return &arg, true
}
