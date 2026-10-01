package parser

import (
	"slices"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
)

func TestDocumentSyntaxPreservesLexicalRecordsAndOwnership(t *testing.T) {
	for _, text := range []string{"", "\ufeffpackage p\r\n// keep\r\nvar x = `a\nb` /* line\ncomment */\n", "package p\nvar x = \"\\xff\"\n\x00", "package p\n/* unterminated"} {
		file := source.NewFile("f", "main.mgo", text)
		file.OriginPath = "original.go"
		scanned := scanner.ScanFile(file)
		document := ParseDocumentFile("sample", file)
		view := document.Syntax()
		if !slices.Equal(view.Tokens, scanned.Tokens) || !slices.Equal(view.Elements, scanned.Elements) {
			t.Fatalf("lexical records changed for %q", text)
		}
		view.Tokens[0].Lexeme = "changed"
		if !slices.Equal(document.Syntax().Tokens, scanned.Tokens) {
			t.Fatal("lexical view mutation reached the document")
		}
	}
}

func TestDocumentSharesLosslessScanAndPartialAST(t *testing.T) {
	source := "package main\n// keep\nvar bad = \x00\nfunc Good() {}\n"
	document := ParseDocument("example/main", "main.mgo", source)
	var rebuilt strings.Builder
	for _, element := range document.Syntax().Elements {
		rebuilt.WriteString(element.Lexeme)
	}
	if rebuilt.String() != source || len(document.Diagnostics) == 0 {
		t.Fatalf("unexpected document: %#v", document)
	}
	found := false
	for _, decl := range document.Program.Files[0].Decls {
		found = found || decl.Kind == ast.DeclFunc && decl.Func.Name == "Good"
	}
	if !found {
		t.Fatalf("partial AST lost later declaration: %#v", document.Program.Files[0].Decls)
	}
}

func TestDocumentOriginPathAndMetadataStayIndependent(t *testing.T) {
	file := source.NewFile("source", "logical.mgo", "package p\nfunc F() int { return 1 }\n")
	file.OriginPath = "original.go"
	document := ParseDocumentFile("sample", file)
	if len(document.Diagnostics) != 0 {
		t.Fatal(document.Diagnostics)
	}
	if document.Program.Files[0].Path != "original.go" {
		t.Fatal("AST did not retain the diagnostic source path")
	}
	want := document.Syntax().Tokens
	document.File.LineStarts[1] = 999
	file.LineStarts[1] = 888
	if !slices.Equal(document.Syntax().Tokens, want) {
		t.Fatal("source metadata mutation changed the lexical document")
	}
}

func TestParseGoEmbedDirectives(t *testing.T) {
	result := ParseSource("example", "embed.mgo", "package example\nimport _ \"embed\"\n//go:embed text/*.txt `file with space.txt`\nvar content []byte\n")
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	decl := result.Program.Files[0].Decls[1]
	if got := decl.Var.EmbedPatterns; len(got) != 2 || got[0] != "text/*.txt" || got[1] != "file with space.txt" {
		t.Fatalf("embed patterns = %#v", got)
	}
}

func TestEmbedSearchPreservesLexicalClassificationAndImportedSpellings(t *testing.T) {
	for _, text := range []string{
		"package example\nvar value = 1\n",
		"package example\nvar text = `//go:embed value.txt`\n",
		"package example\n/* //go:embed value.txt */\nvar value string\n",
		"package example\n//go:embedder value.txt\nvar value string\n",
	} {
		result := ParseSource("example", "embed.mgo", text)
		requireNoDiagnostics(t, result)
		for _, decl := range result.Program.Files[0].Decls {
			if decl.Kind == ast.DeclVar && len(decl.Var.EmbedPatterns) != 0 {
				t.Fatalf("non-directive bound to declaration: %q", text)
			}
		}
	}
	scanned := scanner.Scan("embed.mgo", "package example\n// ordinary comment\nvar value string\n")
	for i := range scanned.Elements {
		if scanned.Elements[i].Kind == scanner.ElementLineComment {
			scanned.Elements[i].Lexeme = "//go:embed value.txt"
		}
	}
	result := ParseScanned("example", scanned)
	requireNoDiagnostics(t, result)
	if got := result.Program.Files[0].Decls[0].Var.EmbedPatterns; !slices.Equal(got, []string{"value.txt"}) {
		t.Fatalf("imported directive spelling lost: %v", got)
	}
}

func TestParseGoEmbedDirectiveBinding(t *testing.T) {
	result := ParseSource("example", "embed.mgo", `package example
import _ "embed"

//go:embed first.txt

// an ordinary comment may separate the directive and declaration
var first string

var (
	//go:embed second.txt
	second string
	third string
)
`)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	var patterns [][]string
	for _, decl := range result.Program.Files[0].Decls {
		if decl.Kind == ast.DeclVar {
			patterns = append(patterns, decl.Var.EmbedPatterns)
		}
	}
	if len(patterns) != 3 || len(patterns[0]) != 1 || patterns[0][0] != "first.txt" || len(patterns[1]) != 1 || patterns[1][0] != "second.txt" || len(patterns[2]) != 0 {
		t.Fatalf("embed binding = %#v", patterns)
	}
}

func TestParseGoEmbedRejectsNonVariableTargets(t *testing.T) {
	for _, sourceText := range []string{
		"package example\n//go:embed value.txt\nconst value = 1\n",
		"package example\n//go:embed value.txt\nfunc value() {}\n",
		"package example\n//go:embed value.txt\nvar (\n\tvalue string\n)\n",
		"package example\nfunc value() {\n//go:embed value.txt\nvar local string\n}\n",
	} {
		result := ParseSource("example", "embed.mgo", sourceText)
		found := false
		for _, diagnostic := range result.Diagnostics {
			found = found || diagnostic.Code == "parser.embed.declaration"
		}
		if !found {
			t.Fatalf("missing declaration diagnostic for %q: %#v", sourceText, result.Diagnostics)
		}
	}
}
