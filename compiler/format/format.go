// Package format implements deterministic formatting over a lossless syntax document.
package format

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

type Result struct {
	Text        string
	Diagnostics []source.Diagnostic
}

type TextEdit struct {
	Span    source.Span
	NewText string
}

func Source(modulePath, path, text string) Result {
	return Document(parser.ParseDocument(modulePath, path, text))
}

func Document(input parser.Document) Result {
	document := input.Syntax()
	if source.HasErrors(document.Diagnostics) {
		return Result{Text: document.File.Text, Diagnostics: append([]source.Diagnostic(nil), document.Diagnostics...)}
	}
	text := render(document.Elements, analyzeSyntax(document))
	diagnostics := validateCandidate(document, text)
	if len(diagnostics) != 0 {
		return Result{Text: document.File.Text, Diagnostics: diagnostics}
	}
	return Result{Text: text}
}

func Range(input parser.Document, selected source.Span) ([]TextEdit, []source.Diagnostic) {
	document := input.Syntax()
	if source.HasErrors(document.Diagnostics) || !selected.Valid() || selected.Start.File != document.File.Path {
		return nil, append([]source.Diagnostic(nil), document.Diagnostics...)
	}
	span := formattingIsland(document, selected)
	islandElements := make([]scanner.Element, 0, len(document.Elements))
	for _, element := range document.Elements {
		if element.Span.Start.Offset >= span.Start.Offset && element.Span.End.Offset <= span.End.Offset {
			islandElements = append(islandElements, element)
		}
	}
	formatted := strings.TrimSuffix(render(islandElements, analyzeSyntax(document)), "\n")
	original := document.File.Text[span.Start.Offset:span.End.Offset]
	if formatted == original {
		return nil, nil
	}
	candidate := document.File.Text[:span.Start.Offset] + formatted + document.File.Text[span.End.Offset:]
	diagnostics := validateCandidate(document, candidate)
	if len(diagnostics) != 0 {
		return nil, diagnostics
	}
	return []TextEdit{{Span: span, NewText: formatted}}, nil
}

func formattingIsland(document parser.Syntax, selected source.Span) source.Span {
	if len(document.Program.Files) == 1 {
		var first, last *source.Span
		for _, declaration := range document.Program.Files[0].Decls {
			span := declaration.Span
			if !span.Valid() || span.End.Offset < selected.Start.Offset || span.Start.Offset > selected.End.Offset {
				continue
			}
			if first == nil {
				value := span
				first = &value
			}
			value := span
			last = &value
		}
		if first != nil {
			span, _ := document.File.Span(first.Start.Offset, last.End.Offset)
			return span
		}
	}
	span, _ := document.File.Span(0, len(document.File.Text))
	return span
}

func OnType(input parser.Document, offset int, typed rune) ([]TextEdit, []source.Diagnostic) {
	document := input.Syntax()
	text := document.File.Text
	if offset <= 0 || offset > len(text) || (typed != '}' && typed != '\n') || text[offset-1] != byte(typed) {
		return nil, nil
	}
	if typed == '\n' {
		// Work on whitespace only, including incomplete programs. Scanner tokens
		// prevent braces inside comments and strings from changing indentation.
		depth := 0
		for _, item := range document.Tokens {
			if item.Span.End.Offset > offset {
				break
			}
			switch item.Kind {
			case token.Lbrace:
				depth++
			case token.Rbrace:
				if depth > 0 {
					depth--
				}
			}
		}
		for _, element := range document.Elements {
			if element.Span.Start.Offset < offset && element.Span.End.Offset > offset {
				return nil, nil
			}
		}
		end := offset
		for end < len(text) && (text[end] == ' ' || text[end] == '\t') {
			end++
		}
		if end < len(text) && text[end] == '}' && depth > 0 {
			depth--
		}
		indent := strings.Repeat("\t", depth)
		if text[offset:end] == indent {
			return nil, nil
		}
		span, _ := document.File.Span(offset, end)
		return []TextEdit{{Span: span, NewText: indent}}, nil
	}
	if source.HasErrors(document.Diagnostics) {
		return nil, nil
	}
	var stack []int
	start := -1
	for _, item := range document.Tokens {
		if item.Span.End.Offset > offset {
			break
		}
		switch item.Kind {
		case token.Lbrace:
			stack = append(stack, item.Span.Start.Offset)
		case token.Rbrace:
			if len(stack) == 0 {
				return nil, nil
			}
			if item.Span.End.Offset == offset {
				start = stack[len(stack)-1]
			}
			stack = stack[:len(stack)-1]
		}
	}
	if start < 0 {
		return nil, nil
	}
	var elements []scanner.Element
	for _, element := range document.Elements {
		if element.Span.Start.Offset >= start && element.Span.End.Offset <= offset {
			elements = append(elements, element)
		}
	}
	formatted := strings.TrimSuffix(render(elements, analyzeSyntax(document)), "\n")
	line := strings.LastIndex(text[:start], "\n") + 1
	endIndent := line
	for endIndent < start && (text[endIndent] == ' ' || text[endIndent] == '\t') {
		endIndent++
	}
	formatted = strings.ReplaceAll(formatted, "\n", "\n"+text[line:endIndent])
	if formatted == text[start:offset] {
		return nil, nil
	}
	if diagnostics := validateCandidate(document, text[:start]+formatted+text[offset:]); len(diagnostics) != 0 {
		return nil, diagnostics
	}
	span, _ := document.File.Span(start, offset)
	return []TextEdit{{Span: span, NewText: formatted}}, nil
}
