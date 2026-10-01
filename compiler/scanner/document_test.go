package scanner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

func TestDocumentOwnsOffsetsAndExpandedViews(t *testing.T) {
	file := source.NewFile("file", "logical.mgo", "package p\r\n// 注释\nvar 值 = 1\n")
	file.OriginPath = "original.go"
	document := ScanDocument(file, Limits{})
	want := document.Syntax()
	file.LineStarts[1] = 999
	view := document.Syntax()
	view.File.LineStarts[1] = 888
	view.Tokens[0].Kind = token.Illegal
	view.Elements[0].Lexeme = "changed"
	if got := document.Syntax(); !reflect.DeepEqual(got, want) {
		t.Fatal("input or expanded view mutation changed the document")
	}
	var rebuilt strings.Builder
	for i := 0; i < document.ElementCount(); i++ {
		record := document.ElementRecord(i)
		rebuilt.WriteString(record.Lexeme)
		if record.Lexeme != file.Text[record.Start:record.End] {
			t.Fatalf("element does not describe its source range: %+v", record)
		}
	}
	if rebuilt.String() != file.Text {
		t.Fatal("lexical tape lost source text")
	}
	for i := 0; i < document.TokenCount(); i++ {
		record, expanded := document.TokenRecord(i), document.Token(i)
		if expanded.Span.Start.File != file.OriginPath || expanded.Span.Start.Offset != record.Start || expanded.Span.End.Offset != record.End {
			t.Fatalf("incorrect expanded source position: %+v", expanded)
		}
	}
}

func BenchmarkScanDocument(b *testing.B) {
	file := source.NewFile("file", "sample.mgo", "package sample\n"+strings.Repeat("// comment\nvar value = 42\n", 100))
	b.ReportAllocs()
	for b.Loop() {
		ScanDocument(file, Limits{})
	}
}

func TestCompactDocumentPositionsMatchSourceOffsets(t *testing.T) {
	for _, text := range []string{"", "package p", "package p\r\nvar x=1\r\n", "package p\nvar x=1 /* first\nsecond */\nvar 字=`a\nb`\n", "\ufeffpackage p\n// 注释\nvar s=\"é\"\n"} {
		file := source.NewFile("file", "logical.mgo", text)
		file.OriginPath = "original.go"
		document := ScanDocument(file, Limits{})
		for _, current := range []Document{document, DocumentFromResult(document.Syntax())} {
			for i := 0; i < current.TokenCount(); i++ {
				item := current.Token(i)
				want, ok := file.Span(item.Span.Start.Offset, item.Span.End.Offset)
				if !ok || item.Span != want {
					t.Fatalf("token %d in %q: %+v, want %+v", i, text, item.Span, want)
				}
			}
			for i := 0; i < current.ElementCount(); i++ {
				item := current.Element(i)
				want, ok := file.Span(item.Span.Start.Offset, item.Span.End.Offset)
				if !ok || item.Span != want {
					t.Fatalf("element %d in %q: %+v, want %+v", i, text, item.Span, want)
				}
			}
		}
	}
}

func TestDocumentRoundTripPreservesTokenSpellingsAndOwnership(t *testing.T) {
	file := source.NewFile("source", "sample.mgo", "x;\r\n/* first\nsecond */ \"text\"")
	view := ScanFile(file)
	view.Tokens[0].Lexeme = "renamed"
	view.Tokens[0].Literal = "decoded"
	duplicate := view.Tokens[0]
	duplicate.Literal = "other"
	view.Tokens = append(view.Tokens, duplicate)
	document := DocumentFromResult(view)
	if got := document.Syntax(); !reflect.DeepEqual(got, view) {
		t.Fatalf("expanded view changed during import: got %+v, want %+v", got, view)
	}
	view.Tokens[0].Literal = "mutated"
	view.Elements[0].Lexeme = "mutated"
	if got := document.Token(0); got.Lexeme != "renamed" || got.Literal != "decoded" {
		t.Fatalf("input mutation reached document: %+v", got)
	}
	if got := document.Element(0).Lexeme; got != "x" {
		t.Fatalf("token spelling changed source element: %q", got)
	}
	for i := 0; i < document.TokenCount(); i++ {
		if document.TokenKind(i) != document.Token(i).Kind {
			t.Fatalf("lookahead and expanded token disagree at %d", i)
		}
	}
}

func TestDocumentSyntheticSemicolonsAndLimitedViews(t *testing.T) {
	for _, input := range []string{"x", "x;", "x\r\ny", "x /* a\nb\nc */ y", "x\n/* a */\ny"} {
		for _, limit := range []int{1, 2, 100} {
			document := ScanDocument(source.NewFile("file", "sample.mgo", input), Limits{MaxTokens: limit})
			view := document.Syntax()
			var text strings.Builder
			for _, element := range view.Elements {
				text.WriteString(element.Lexeme)
			}
			if !strings.HasPrefix(input, text.String()) {
				t.Fatalf("limited scan lost source ordering: %q -> %q", input, text.String())
			}
			for _, item := range view.Tokens {
				if item.Kind == token.Semicolon && item.Lexeme != ";" && item.Lexeme != "\n" {
					t.Fatalf("incorrect semicolon spelling: %+v", item)
				}
				want, valid := view.File.Span(item.Span.Start.Offset, item.Span.End.Offset)
				if !valid || item.Span != want {
					t.Fatalf("incorrect source position in %q: %+v, want %+v", input, item, want)
				}
			}
			if roundtrip := DocumentFromResult(view).Syntax(); !reflect.DeepEqual(roundtrip, view) {
				t.Fatalf("limited lexical view changed during import: %q, limit %d", input, limit)
			}
		}
	}
}
