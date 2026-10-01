package format_test

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/format"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
)

func TestSourceIsIdempotentAndPreservesCommentsAndLiterals(t *testing.T) {
	source := "package   main\n// keep me\nfunc main( ){\nvalue:=`a  b`\n_ = value\n}\n"
	first := format.Source("example/main", "main.mgo", source)
	if len(first.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", first.Diagnostics)
	}
	second := format.Source("example/main", "main.mgo", first.Text)
	if first.Text != second.Text {
		t.Fatalf("formatter is not idempotent:\nfirst:\n%s\nsecond:\n%s", first.Text, second.Text)
	}
	if first.Text == source || !contains(first.Text, "// keep me") || !contains(first.Text, "`a  b`") {
		t.Fatalf("formatted text = %q", first.Text)
	}
}

func TestGenericMethodsAndPromotedLiteralsRoundTrip(t *testing.T) {
	text := "package p\ntype Inner struct{X int};type Box[T any] struct{Inner;V T}\nfunc(b Box[T])Map[U any](f func(T)U)U{return f(b.V)}\nfunc Main()int{return Box[int]{X:1,V:2}.Map(func(x int)int{return x})}\n"
	formatted := format.Source("example", "methods.mgo", text)
	if len(formatted.Diagnostics) != 0 {
		t.Fatal(formatted.Diagnostics)
	}
	parsed := parser.ParseSource("example", "methods.mgo", formatted.Text)
	if len(parsed.Diagnostics) != 0 {
		t.Fatal(parsed.Diagnostics)
	}
	if second := format.Source("example", "methods.mgo", formatted.Text); second.Text != formatted.Text {
		t.Fatal("unstable generic method format")
	}
}

func TestSourcePreservesInlineCommentAfterBlockOpen(t *testing.T) {
	input := "package main\nconst value = 1 // keep declaration inline\ntype mode uint\nconst (\n\tfirst mode = iota // keep grouped inline\n\tsecond             // keep implicit inline\n)\nvar (\n\tdata = []byte(\"x\") // keep var inline\n)\nfunc run(ok bool, value int) {\nif ok { // keep block inline\nreturn\n}\nswitch value {\ncase 1: // keep clause inline\nreturn\n}\n}\n"
	result := format.Source("example/main", "main.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	if !strings.Contains(result.Text, "const value = 1 // keep declaration inline\n") ||
		!strings.Contains(result.Text, "first mode = iota // keep grouped inline\n") ||
		!strings.Contains(result.Text, "second // keep implicit inline\n") ||
		!strings.Contains(result.Text, "data = []byte(\"x\") // keep var inline\n") ||
		!strings.Contains(result.Text, "if ok { // keep block inline\n") ||
		!strings.Contains(result.Text, "case 1: // keep clause inline\n") {
		t.Fatalf("formatter moved inline comment:\n%s", result.Text)
	}
}

func TestSourceKeepsCommentAnchorWhenRemovingTrailingSemicolons(t *testing.T) {
	input := "package sample\nfunc first() { if true { return; }; };\n// Second documents second.\nfunc second() {}\n"
	result := format.Source("example/sample", "sample.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	if !strings.Contains(result.Text, "}\n\n// Second documents second.\nfunc second()") {
		t.Fatalf("formatted comment anchor changed:\n%s", result.Text)
	}
}

func TestSourceLeavesInvalidDocumentUnchanged(t *testing.T) {
	source := "package main\nfunc broken( {\n"
	result := format.Source("example/main", "main.mgo", source)
	if result.Text != source || len(result.Diagnostics) == 0 {
		t.Fatalf("format invalid result = %#v", result)
	}
}

func TestSourceIndentsElidedCompositeFromItsOwnBrace(t *testing.T) {
	input := "package sample\nvar values = map[string]map[string]int{\n\"outer\": {\n\"inner\": 1,\n},\n}\n"
	formatted := format.Source("sample", "main.mgo", input)
	if len(formatted.Diagnostics) != 0 || !strings.Contains(formatted.Text, "\t\"outer\": {\n\t\t\"inner\": 1,\n\t},") {
		t.Fatalf("nested composite formatting: %v\n%s", formatted.Diagnostics, formatted.Text)
	}
	again := format.Source("sample", "main.mgo", formatted.Text)
	if len(again.Diagnostics) != 0 || again.Text != formatted.Text {
		t.Fatalf("formatting is not stable: %v\n%s", again.Diagnostics, again.Text)
	}
}

func TestSourcePreservesImportsAndFormatsUnaryOperators(t *testing.T) {
	input := "package main\nimport \"z\"\nimport \"a\"\nfunc main() { _ = -1 - -2 }\n"
	result := format.Source("example/main", "main.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	if !contains(result.Text, "import \"z\"\nimport \"a\"") || !contains(result.Text, "_ = -1 - -2") {
		t.Fatalf("formatted text = %q", result.Text)
	}
}

func TestSourceFormatsSyntaxRoles(t *testing.T) {
	input := `package sample
type Pair[T any] struct { Left T; Right T }
type Reader interface { Read([]byte) (int,error) }
func choose[T any](channel chan T, values []T) T {
for index:=0;index<len(values);index++ { values[index]=values[index]+values[index] }
switch { case len(values)>0:return values[0];default: }
select { case value:=<-channel:return value;default:return Pair[T]{Left:values[0]}.Left }
}

func TestSourceFormatsPointerTypesWithoutLeadingSpace(t *testing.T) {
	input := "package sample\nvar trees map[string] *Tree\nfunc dereference(value *int) int { return *value }\n"
	result := format.Source("example/sample", "sample.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	if !strings.Contains(result.Text, "map[string]*Tree") || !strings.Contains(result.Text, "return *value") {
		t.Fatalf("formatted pointer types and expressions incorrectly:\n%s", result.Text)
	}
}
`
	first := format.Source("example/sample", "sample.mgo", input)
	if len(first.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", first.Diagnostics)
	}
	wants := []string{
		"type Pair[T any] struct {",
		"Read([]byte) (int, error)",
		"for index := 0; index < len(values); index++ {",
		"value := <-channel",
		"Pair[T]{Left: values[0]}.Left",
	}
	for _, want := range wants {
		if !strings.Contains(first.Text, want) {
			t.Fatalf("formatted source does not contain %q:\n%s", want, first.Text)
		}
	}
	second := format.Source("example/sample", "sample.mgo", first.Text)
	if len(second.Diagnostics) != 0 || second.Text != first.Text {
		t.Fatalf("second format changed output: diagnostics=%v\n%s", second.Diagnostics, second.Text)
	}
}

func TestSourcePreservesBuildAndEmbedDirectives(t *testing.T) {
	input := "//go:build minigo\n\npackage assets\nimport _ \"embed\"\n//go:embed text/*.txt `file with space.txt`\nvar content []byte\n"
	result := format.Source("example/assets", "assets.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	for _, directive := range []string{"//go:build minigo", "//go:embed text/*.txt `file with space.txt`"} {
		if !strings.Contains(result.Text, directive) {
			t.Fatalf("formatted source lost %q:\n%s", directive, result.Text)
		}
	}
}

func TestSourceKeepsEmptyTypesAndBlocksCompact(t *testing.T) {
	input := "package sample\ntype empty struct{\n}\nfunc receive() <-chan struct{\n} { return nil }\n"
	result := format.Source("example/sample", "sample.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	for _, want := range []string{"type empty struct{}", "func receive() <-chan struct{} {"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("formatted source does not contain %q:\n%s", want, result.Text)
		}
	}
}

func TestSourceFormatsVariadicParametersAndLabels(t *testing.T) {
	input := "package sample\nfunc collect(values...int){ start: values=append(values, values...); if len(values)>2 { goto start } }\n"
	result := format.Source("example/sample", "sample.mgo", input)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("format diagnostics = %#v", result.Diagnostics)
	}
	for _, want := range []string{"collect(values ...int)", "start:\n", "append(values, values...)"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("formatted source does not contain %q:\n%s", want, result.Text)
		}
	}
}

func TestRangeFormatsOnlyIntersectingDeclarations(t *testing.T) {
	source := "package main\nfunc First( ) { _ = 1 }\nfunc Second( ) { value:=2; _=value }\n"
	document := parser.ParseDocument("example/main", "main.mgo", source)
	selected, ok := document.File.Span(55, 55)
	if !ok {
		t.Fatal("selection is outside source")
	}
	edits, diagnostics := format.Range(document, selected)
	if len(diagnostics) != 0 || len(edits) != 1 {
		t.Fatalf("range result: edits=%#v diagnostics=%#v", edits, diagnostics)
	}
	if edits[0].Span.Start.Offset <= len("package main\n") || edits[0].NewText == source[edits[0].Span.Start.Offset:edits[0].Span.End.Offset] {
		t.Fatalf("range edit = %#v", edits[0])
	}
	formatted := source[:edits[0].Span.Start.Offset] + edits[0].NewText + source[edits[0].Span.End.Offset:]
	if !contains(formatted, "func First( ) { _ = 1 }") || !contains(formatted, "func Second()") {
		t.Fatalf("formatted range = %q", formatted)
	}
}

func TestOnTypeFormatsOnlyClosedBlock(t *testing.T) {
	input := "package main\nfunc main( ){ value:=[]int{1,2}; _=value }\n"
	document := parser.ParseDocument("example/main", "main.mgo", input)
	edits, diagnostics := format.OnType(document, len(input)-1, '}')
	if len(diagnostics) != 0 || len(edits) != 1 {
		t.Fatalf("on-type result: edits=%#v diagnostics=%#v", edits, diagnostics)
	}
	if edits[0].Span.Start.Offset != len("package main\nfunc main( )") || edits[0].Span.End.Offset != len(input)-1 {
		t.Fatalf("on-type span = %#v", edits[0].Span)
	}
	formatted := input[:edits[0].Span.Start.Offset] + edits[0].NewText + input[edits[0].Span.End.Offset:]
	if reparsed := parser.ParseDocument("example/main", "main.mgo", formatted); source.HasErrors(reparsed.Diagnostics) {
		t.Fatalf("on-type output is invalid: %v", reparsed.Diagnostics)
	}
}

func TestOnTypeIndentsIncompleteBlock(t *testing.T) {
	input := "package main\nfunc main() {\n"
	edits, diagnostics := format.OnType(parser.ParseDocument("main", "main.mgo", input), len(input), '\n')
	if len(diagnostics) != 0 || len(edits) != 1 || edits[0].NewText != "\t" || edits[0].Span.Start.Offset != len(input) || edits[0].Span.End.Offset != len(input) {
		t.Fatalf("indent = %#v, %v", edits, diagnostics)
	}
	for _, trigger := range []rune{'x', ';', '}'} {
		if edits, _ := format.OnType(parser.ParseDocument("main", "main.mgo", input), len(input), trigger); len(edits) != 0 {
			t.Fatalf("unsafe edit for %q: %#v", trigger, edits)
		}
	}
}

func TestOnTypePreservesUnicodeDirectivesAndOuterCRLF(t *testing.T) {
	prefix := "//go:build editor\r\n\r\npackage main\r\nfunc prior( ){_=\"😀\"}\r\nfunc Main() "
	suffix := "\r\n// trailing 😀\r\n"
	input := prefix + "{x:=1;_=x}" + suffix
	offset := len(input) - len(suffix)
	edits, diagnostics := format.OnType(parser.ParseDocument("main", "main.mgo", input), offset, '}')
	if len(diagnostics) != 0 || len(edits) != 1 {
		t.Fatalf("on type: %#v %#v", edits, diagnostics)
	}
	edit := edits[0]
	if edit.Span.Start.Offset < len(prefix) || edit.Span.End.Offset > offset {
		t.Fatal("edit escaped closed block")
	}
	text := input[:edit.Span.Start.Offset] + edit.NewText + input[edit.Span.End.Offset:]
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) {
		t.Fatal("outer CRLF or non-BMP text changed")
	}
	newOffset := len(text) - len(suffix)
	again, _ := format.OnType(parser.ParseDocument("main", "main.mgo", text), newOffset, '}')
	for _, edit := range again {
		if text[edit.Span.Start.Offset:edit.Span.End.Offset] != edit.NewText {
			t.Fatal("on-type is not idempotent")
		}
	}
}

func contains(text, part string) bool {
	for index := 0; index+len(part) <= len(text); index++ {
		if text[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
