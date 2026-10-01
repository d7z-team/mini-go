// Package parser parses Mini-Go source and owns its immutable lossless document.
package parser

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
)

type Document struct {
	File        source.File
	Program     ast.Program
	Diagnostics []source.Diagnostic
	NodeCount   int
	lexical     scanner.Document
}

// Syntax is an expanded lexical view owned by one source tooling operation.
type Syntax struct {
	Document
	Elements []scanner.Element
	Tokens   []scanner.Token
}

// Syntax reconstructs lossless lexical records without rescanning source text.
func (d Document) Syntax() Syntax {
	view := d.lexical.Syntax()
	return Syntax{Document: d, Elements: view.Elements, Tokens: view.Tokens}
}

func ParseDocument(modulePath, path, text string) Document {
	return ParseDocumentFile(modulePath, source.NewFile("file.0", path, text))
}

func ParseDocumentFile(modulePath string, file source.File) Document {
	return ParseDocumentFileWithLimits(modulePath, file, Limits{})
}

func ParseDocumentFileWithLimits(modulePath string, file source.File, limits Limits) Document {
	limits = normalizeLimits(limits)
	scanned := scanner.ScanDocument(file, limits.Scanner)
	parsed := parseScanned(modulePath, scanned, limits)
	document := Document{
		File:        scanned.File(),
		Program:     parsed.Program,
		Diagnostics: append([]source.Diagnostic(nil), parsed.Diagnostics...),
		NodeCount:   parsed.NodeCount,
		lexical:     scanned,
	}
	return document
}
