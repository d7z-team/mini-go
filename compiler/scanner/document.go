package scanner

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

// Document owns compact lexical records. Accessors return values, so parser
// lookahead and tooling views cannot mutate a shared document.
type Document struct {
	file        source.File
	records     lexicalTape
	tokens      []int
	elements    []int
	text        map[int]lexicalText
	diagnostics []source.Diagnostic
}

// Tokens and lossless elements index the same tape. Text normally borrows the
// source range; only externally supplied spellings need an override.
type lexicalTape struct {
	kinds     []token.Kind
	elements  []ElementKind
	positions []int // start, end and starting line for each record
}

func (tape *lexicalTape) appendRecord(kind token.Kind, element ElementKind, start, end, line int) {
	tape.kinds = append(tape.kinds, kind)
	tape.elements = append(tape.elements, element)
	tape.positions = append(tape.positions, start, end, line)
}

type lexicalText struct {
	lexeme, literal string
}

type TokenRecord struct {
	Kind       token.Kind
	Lexeme     string
	Literal    string
	Start, End int
	line       int
}

type ElementRecord struct {
	Kind       ElementKind
	Token      token.Kind
	Lexeme     string
	Start, End int
	line       int
}

func ScanDocument(file source.File, limits Limits) Document {
	return scanDocument(file, limits, false)
}

// DocumentFromResult takes a copy of an expanded lexical view.
func DocumentFromResult(result Result) Document {
	file := source.InitializeFile(result.File)
	file.LineStarts = append([]int(nil), file.LineStarts...)
	document := Document{
		file: file, tokens: make([]int, len(result.Tokens)),
		elements:    make([]int, len(result.Elements)),
		diagnostics: append([]source.Diagnostic(nil), result.Diagnostics...),
	}
	type tokenKey struct {
		kind       token.Kind
		start, end int
	}
	shared := make(map[tokenKey]int)
	for i, item := range result.Elements {
		position, _ := file.Position(item.Span.Start.Offset)
		index := len(document.records.kinds)
		document.elements[i] = index
		document.records.appendRecord(item.Token, item.Kind, item.Span.Start.Offset, item.Span.End.Offset, position.Line-1)
		if document.lexeme(index) != item.Lexeme {
			if document.text == nil {
				document.text = make(map[int]lexicalText)
			}
			document.text[index] = lexicalText{lexeme: item.Lexeme}
		}
		if item.Kind == ElementToken {
			shared[tokenKey{kind: item.Token, start: item.Span.Start.Offset, end: item.Span.End.Offset}] = index
		}
	}
	for i, item := range result.Tokens {
		key := tokenKey{kind: item.Kind, start: item.Span.Start.Offset, end: item.Span.End.Offset}
		index, exists := shared[key]
		if !exists || document.lexeme(index) != item.Lexeme || document.text[index].literal != item.Literal {
			position, _ := file.Position(key.start)
			index = len(document.records.kinds)
			document.records.appendRecord(item.Kind, "", key.start, key.end, position.Line-1)
		}
		document.tokens[i] = index
		if document.lexeme(index) != item.Lexeme || item.Literal != "" {
			if document.text == nil {
				document.text = make(map[int]lexicalText)
			}
			document.text[index] = lexicalText{lexeme: item.Lexeme, literal: item.Literal}
		}
	}
	return document
}

func (d Document) File() source.File {
	file := d.file
	file.LineStarts = append([]int(nil), file.LineStarts...)
	return file
}

func (d Document) Diagnostics() []source.Diagnostic {
	return append([]source.Diagnostic(nil), d.diagnostics...)
}

// MayContainLexeme is a conservative filter for lexical searches. A positive
// result still requires checking element kinds and boundaries. Imported views
// may supply spellings that do not occur in the original source text.
func (d Document) MayContainLexeme(fragment string) bool {
	if strings.Contains(d.file.Text, fragment) {
		return true
	}
	for _, text := range d.text {
		if strings.Contains(text.lexeme, fragment) {
			return true
		}
	}
	return false
}

func (d Document) TokenCount() int { return len(d.tokens) }

func (d Document) ElementCount() int { return len(d.elements) }

// ElementKind reads trivia classification without expanding spelling or spans.
func (d Document) ElementKind(index int) ElementKind {
	return d.records.elements[d.elements[index]]
}

// TokenKind reads lookahead without expanding text or source positions.
func (d Document) TokenKind(index int) token.Kind {
	return d.records.kinds[d.tokens[index]]
}

func (d Document) TokenRecord(index int) TokenRecord {
	index = d.tokens[index]
	position := index * 3
	return TokenRecord{Kind: d.records.kinds[index], Lexeme: d.lexeme(index), Literal: d.text[index].literal, Start: d.records.positions[position], End: d.records.positions[position+1], line: d.records.positions[position+2]}
}

func (d Document) ElementRecord(index int) ElementRecord {
	index = d.elements[index]
	position := index * 3
	return ElementRecord{Kind: d.records.elements[index], Token: d.records.kinds[index], Lexeme: d.lexeme(index), Start: d.records.positions[position], End: d.records.positions[position+1], line: d.records.positions[position+2]}
}

func (d Document) lexeme(index int) string {
	if text, exists := d.text[index]; exists {
		return text.lexeme
	}
	if d.records.elements[index] == "" && d.records.kinds[index] == token.Semicolon {
		return "\n"
	}
	position := index * 3
	start, end := d.records.positions[position], d.records.positions[position+1]
	if start < 0 || end < start || end > len(d.file.Text) {
		return ""
	}
	return d.file.Text[start:end]
}

func (d Document) Token(index int) Token {
	item := d.TokenRecord(index)
	return Token{Kind: item.Kind, Lexeme: item.Lexeme, Literal: item.Literal, Span: d.span(item.Start, item.End, item.line)}
}

func (d Document) Element(index int) Element {
	item := d.ElementRecord(index)
	return Element{Kind: item.Kind, Token: item.Token, Lexeme: item.Lexeme, Span: d.span(item.Start, item.End, item.line)}
}

// The scanner records the start line while moving monotonically through the
// document. Most lexical ranges fit on that line; only multiline endings need
// a position search when a caller requests an expanded view.
func (d Document) span(start, end, line int) source.Span {
	if line < 0 || line >= len(d.file.LineStarts) || start < 0 || end < start || end > len(d.file.Text) || d.file.Path == "" {
		return source.Span{}
	}
	filename := d.file.Path
	if d.file.OriginPath != "" {
		filename = d.file.OriginPath
	}
	first := source.Position{File: filename, Offset: start, Line: line + 1, Column: start - d.file.LineStarts[line]}
	last := first
	last.Offset, last.Column = end, end-d.file.LineStarts[line]
	if line+1 < len(d.file.LineStarts) && end >= d.file.LineStarts[line+1] {
		last, _ = d.file.Position(end)
	}
	return source.Span{Start: first, End: last}
}

func (d Document) Syntax() Result {
	result := Result{File: d.File(), Diagnostics: d.Diagnostics(), Tokens: make([]Token, len(d.tokens)), Elements: make([]Element, len(d.elements))}
	for i := range result.Tokens {
		result.Tokens[i] = d.Token(i)
	}
	for i := range result.Elements {
		result.Elements[i] = d.Element(i)
	}
	return result
}
