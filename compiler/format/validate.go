package format

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

type triviaAnchor struct {
	kind       scanner.ElementKind
	lexeme     string
	previous   int
	next       int
	afterCode  bool
	beforeCode bool
}

func validateCandidate(original parser.Syntax, text string) []source.Diagnostic {
	reparsed := parser.ParseDocument(original.Program.ModulePath, original.File.Path, text).Syntax()
	if source.HasErrors(reparsed.Diagnostics) {
		message := "formatter produced source that cannot be parsed"
		if len(reparsed.Diagnostics) != 0 {
			message += ": " + reparsed.Diagnostics[0].Message
		}
		return []source.Diagnostic{formatDiagnostic(original.File, "format.output.syntax", message)}
	}
	if !equalLogicalTokens(original.Tokens, reparsed.Tokens) {
		return []source.Diagnostic{formatDiagnostic(original.File, "format.output.tokens", "formatter changed the logical token stream")}
	}
	if detail, ok := triviaAnchorDifference(original.Elements, reparsed.Elements); ok {
		return []source.Diagnostic{formatDiagnostic(original.File, "format.output.comments", "formatter changed a comment, BOM, or its syntax anchor: "+detail)}
	}
	if strings.Join(embedBindings(original.Program), "\x00") != strings.Join(embedBindings(reparsed.Program), "\x00") {
		return []source.Diagnostic{formatDiagnostic(original.File, "format.output.directives", "formatter changed //go:embed directive binding")}
	}
	return nil
}

func equalLogicalTokens(left, right []scanner.Token) bool {
	left = significantTokens(left)
	right = significantTokens(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind {
			return false
		}
		leftLexeme := left[index].Lexeme
		rightLexeme := right[index].Lexeme
		if left[index].Kind == token.Semicolon {
			leftLexeme = ";"
			rightLexeme = ";"
		}
		if leftLexeme != rightLexeme {
			return false
		}
	}
	return true
}

func significantTokens(tokens []scanner.Token) []scanner.Token {
	out := make([]scanner.Token, 0, len(tokens))
	for index, item := range tokens {
		if item.Kind == token.Semicolon {
			next := token.EOF
			for lookahead := index + 1; lookahead < len(tokens); lookahead++ {
				if tokens[lookahead].Kind != token.Semicolon {
					next = tokens[lookahead].Kind
					break
				}
			}
			if next == token.Rbrace || next == token.EOF {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func triviaAnchorDifference(left, right []scanner.Element) (string, bool) {
	leftAnchors := collectTriviaAnchors(left)
	rightAnchors := collectTriviaAnchors(right)
	if len(leftAnchors) != len(rightAnchors) {
		return fmt.Sprintf("count changed from %d to %d", len(leftAnchors), len(rightAnchors)), true
	}
	for index := range leftAnchors {
		if leftAnchors[index] != rightAnchors[index] {
			return fmt.Sprintf("anchor %d changed from %#v to %#v", index, leftAnchors[index], rightAnchors[index]), true
		}
	}
	return "", false
}

func collectTriviaAnchors(elements []scanner.Element) []triviaAnchor {
	tokenKinds := make([]token.Kind, 0, len(elements))
	for _, element := range elements {
		if element.Kind == scanner.ElementToken {
			tokenKinds = append(tokenKinds, element.Token)
		}
	}
	include := make([]bool, len(tokenKinds))
	for index, kind := range tokenKinds {
		include[index] = kind != token.Semicolon
	}
	tokenLines := make([]int, 0, len(tokenKinds))
	rawTokenIndex := 0
	for _, element := range elements {
		if element.Kind == scanner.ElementToken {
			if include[rawTokenIndex] {
				tokenLines = append(tokenLines, element.Span.Start.Line)
			}
			rawTokenIndex++
		}
	}
	anchors := []triviaAnchor{}
	tokenIndex := 0
	rawTokenIndex = 0
	for _, element := range elements {
		if element.Kind == scanner.ElementToken {
			if include[rawTokenIndex] {
				tokenIndex++
			}
			rawTokenIndex++
			continue
		}
		if element.Kind != scanner.ElementLineComment && element.Kind != scanner.ElementBlockComment && element.Kind != scanner.ElementBOM {
			continue
		}
		anchor := triviaAnchor{kind: element.Kind, lexeme: element.Lexeme, previous: tokenIndex - 1, next: tokenIndex}
		if tokenIndex > 0 {
			anchor.afterCode = tokenLines[tokenIndex-1] == element.Span.Start.Line
		}
		if tokenIndex < len(tokenLines) {
			anchor.beforeCode = tokenLines[tokenIndex] == element.Span.End.Line
		}
		anchors = append(anchors, anchor)
	}
	return anchors
}

func embedBindings(program ast.Program) []string {
	bindings := []string{}
	var collectDecl func(ast.Decl)
	var collectBlock func(ast.BlockStmt)
	var collectStmt func(ast.Statement)
	collectDecl = func(decl ast.Decl) {
		var value ast.ValueDecl
		switch decl.Kind {
		case ast.DeclVar:
			value = *decl.Var
		case ast.DeclFunc:
			collectBlock(decl.Func.Body)
		default:
			return
		}
		if len(value.EmbedPatterns) != 0 {
			bindings = append(bindings, strings.Join(value.Names, ",")+"="+strings.Join(value.EmbedPatterns, ","))
		}
	}
	collectStmt = func(stmt ast.Statement) {
		for _, decl := range stmt.Decls {
			collectDecl(decl)
		}
		collectBlock(stmt.Body)
		if stmt.Init != nil {
			collectStmt(*stmt.Init)
		}
		if stmt.Post != nil {
			collectStmt(*stmt.Post)
		}
		if stmt.Else != nil {
			collectStmt(*stmt.Else)
		}
		for _, clause := range stmt.Cases {
			if clause.Comm != nil {
				collectStmt(*clause.Comm)
			}
			collectBlock(clause.Body)
		}
	}
	collectBlock = func(block ast.BlockStmt) {
		for _, stmt := range block.Stmts {
			collectStmt(stmt)
		}
	}
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			collectDecl(decl)
		}
	}
	return bindings
}

func formatDiagnostic(file source.File, code source.DiagnosticCode, message string) source.Diagnostic {
	span, ok := file.Span(0, len(file.Text))
	if !ok {
		span = source.Span{}
	}
	return source.Diagnostic{Code: code, Severity: source.SeverityError, Message: message, Primary: span}
}
