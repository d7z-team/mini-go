package scanner

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

func TestLexicalCapacityCanExceedDefaultBudgets(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		file := source.NewFile("sample", "large.mgo", strings.Repeat(";", DefaultMaxTokens+1))
		for _, limit := range []int{0, DefaultMaxTokens + 2} {
			document := ScanDocument(file, Limits{MaxTokens: limit})
			if limit == 0 {
				if !source.HasErrors(document.Diagnostics()) {
					t.Fatal("expected token budget diagnostic")
				}
			} else if diagnostics := document.Diagnostics(); len(diagnostics) != 0 || document.TokenCount() != DefaultMaxTokens+2 {
				t.Fatalf("raised token budget: tokens=%d diagnostics=%v", document.TokenCount(), diagnostics)
			}
		}
	})
	t.Run("diagnostics", func(t *testing.T) {
		file := source.NewFile("sample", "invalid.mgo", strings.Repeat("@ ", DefaultMaxDiagnostics+1))
		document := ScanDocument(file, Limits{MaxDiagnostics: DefaultMaxDiagnostics + 2})
		for _, diagnostic := range document.Diagnostics() {
			if diagnostic.Code == source.DiagnosticTruncated {
				t.Fatalf("raised diagnostic budget truncated: %v", document.Diagnostics())
			}
		}
		if len(document.Diagnostics()) != DefaultMaxDiagnostics+1 {
			t.Fatalf("expected every invalid token to be diagnosed: %v", document.Diagnostics())
		}
	})
}

func TestLexicalViewsExpandSourceSpansInEitherDirection(t *testing.T) {
	for _, text := range []string{"", "plain", "a\r\n中文\n\xfflast\n", "\n\n"} {
		file := source.NewFile("sample", "sample.mgo", text)
		document := ScanDocument(file, Limits{})
		for i := 0; i < document.TokenCount(); i++ {
			for _, index := range []int{i, document.TokenCount() - i - 1, i} {
				record := document.TokenRecord(index)
				want, _ := file.Span(record.Start, record.End)
				if got := document.Token(index).Span; got != want {
					t.Fatalf("%q token %d span %+v, want %+v", text, index, got, want)
				}
			}
		}
	}
}

func TestScanPackageFunctionTokens(t *testing.T) {
	source := "package main\n\nfunc Add(a int, b int) int {\n\treturn a + b\n}\n"
	result := Scan("main.mgo", source)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	want := []token.Kind{
		token.Package, token.Ident, token.Semicolon,
		token.Func, token.Ident, token.Lparen, token.Ident, token.Ident, token.Comma, token.Ident, token.Ident, token.Rparen, token.Ident, token.Lbrace,
		token.Return, token.Ident, token.Add, token.Ident, token.Semicolon,
		token.Rbrace, token.Semicolon, token.EOF,
	}
	assertKinds(t, result.Tokens, want)
	if got := result.Tokens[0].Span.Start; got.Line != 1 || got.Column != 0 {
		t.Fatalf("first token position = %+v, want line 1 column 0", got)
	}
	returnToken := result.Tokens[14]
	if returnToken.Span.Start.Line != 4 || returnToken.Span.Start.Column != 1 {
		t.Fatalf("return position = %+v, want line 4 column 1", returnToken.Span.Start)
	}
}

func TestScanOperatorsAndDelimiters(t *testing.T) {
	source := "func f(){ a := <-ch; b++; c--; d <<= 2; e &^= 1; if a == b || c != d && e >= 0 { f(...); _ = ~mask } }"
	result := Scan("ops.mgo", source)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	for _, kind := range []token.Kind{
		token.Define, token.Arrow, token.Inc, token.Dec, token.ShlAssign,
		token.AndNotAssign, token.Eq, token.Lor, token.Ne, token.Land,
		token.Ge, token.Ellipsis, token.Tilde,
	} {
		if !hasKind(result.Tokens, kind) {
			t.Fatalf("expected token kind %s in %#v", kind, kinds(result.Tokens))
		}
	}
}

func TestScanLiteralsAndUnicodeIdentifiers(t *testing.T) {
	source := "package main\nvar 世界 = []string{\"hi\\n\", `raw\ntext`, '界', 0x1.fp2, 123i}\n"
	result := Scan("literals.mgo", source)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	if !hasLexeme(result.Tokens, token.Ident, "世界") {
		t.Fatalf("expected Unicode identifier, got %#v", result.Tokens)
	}
	for _, kind := range []token.Kind{token.String, token.Char, token.Float, token.Imag} {
		if !hasKind(result.Tokens, kind) {
			t.Fatalf("expected literal kind %s in %#v", kind, kinds(result.Tokens))
		}
	}
}

func TestScannerASCIIRunsPreserveUnicodeBoundariesAndTrivia(t *testing.T) {
	name := strings.Repeat("a_1Z", 2049) + "世界" + "_٢_tail"
	padding := strings.Repeat(" \t\r", 3000)
	text := "package main\n" + padding + "var " + name + " = 1\n"
	result := Scan("runs.mgo", text)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	if !hasLexeme(result.Tokens, token.Ident, name) {
		t.Fatal("mixed ASCII and Unicode identifier was split")
	}
	file := source.NewFile("sample", "runs.mgo", text)
	for _, tok := range result.Tokens {
		want, ok := file.Span(tok.Span.Start.Offset, tok.Span.End.Offset)
		if !ok || tok.Span != want {
			t.Fatalf("token %s span %+v, want %+v", tok.Kind, tok.Span, want)
		}
	}
}

func TestScanIdentifierStopsBeforeInvalidUTF8(t *testing.T) {
	result := Scan("invalid-identifier.mgo", "name_\xfftail")
	if !hasDiagnostic(result.Diagnostics, "scanner.utf8.invalid") {
		t.Fatalf("expected invalid UTF-8 diagnostic, got %+v", result.Diagnostics)
	}
	for _, want := range []struct {
		index int
		kind  token.Kind
		text  string
		start int
		end   int
	}{
		{0, token.Ident, "name_", 0, 5},
		{1, token.Illegal, "\xff", 5, 6},
		{2, token.Ident, "tail", 6, 10},
	} {
		got := result.Tokens[want.index]
		if got.Kind != want.kind || got.Lexeme != want.text || got.Span.Start.Offset != want.start || got.Span.End.Offset != want.end {
			t.Fatalf("token %d = %+v, want %s %q [%d:%d]", want.index, got, want.kind, want.text, want.start, want.end)
		}
	}
}

func BenchmarkScanASCIIIdentifiers(b *testing.B) {
	text := "package main\n" + strings.Repeat("var alpha_0123456789 = beta_0123456789\n", 256)
	b.ReportAllocs()
	for b.Loop() {
		Scan("bench.mgo", text)
	}
}

func TestScanNumericLiteralUnderscores(t *testing.T) {
	valid := []string{
		"1_000",
		"0b_1010",
		"0o_755",
		"0x_FF",
		"0x1e_2",
		"1_2.3_4e+5_6",
		"1_2i",
	}
	for _, literal := range valid {
		result := Scan("valid-"+literal+".mgo", literal)
		if len(result.Diagnostics) != 0 {
			t.Fatalf("%s: unexpected diagnostics: %+v", literal, result.Diagnostics)
		}
	}

	invalid := []string{
		"1__2",
		"1_",
		"1_.2",
		"1e_2",
		"0x1p_2",
	}
	for _, literal := range invalid {
		result := Scan("invalid-"+literal+".mgo", literal)
		if !hasDiagnostic(result.Diagnostics, "scanner.number.underscore") {
			t.Fatalf("%s: expected scanner.number.underscore, got %+v", literal, result.Diagnostics)
		}
	}
}

func TestScanRejectsHexFloatWithoutExponentAndInvalidOctalDigits(t *testing.T) {
	for _, test := range []struct {
		literal string
		code    string
	}{
		{literal: "0x1.f", code: "scanner.number.hex_exponent"},
		{literal: "08", code: "scanner.number.octal_digit"},
		{literal: "09i", code: "scanner.number.octal_digit"},
		{literal: "0_8", code: "scanner.number.octal_digit"},
		{literal: "0__7", code: "scanner.number.underscore"},
		{literal: "0b٢", code: "scanner.number.digits"},
		{literal: "123世界", code: "scanner.number.suffix"},
	} {
		result := Scan("invalid-number.mgo", test.literal)
		if !hasDiagnostic(result.Diagnostics, test.code) {
			t.Fatalf("%s: expected %s, got %+v", test.literal, test.code, result.Diagnostics)
		}
	}
	for _, literal := range []string{"0x1.fp2", "0x.8p0", "0755", "0_755", "08.0", "08e1", "0_8e1i", "0b_101", "0o_77", "0x_Ff", ".5", ".5i"} {
		result := Scan("valid-number.mgo", literal)
		if len(result.Diagnostics) != 0 {
			t.Fatalf("%s: unexpected diagnostics: %+v", literal, result.Diagnostics)
		}
	}
}

func TestScanSourceTextBOMAndNUL(t *testing.T) {
	result := Scan("bom.mgo", "\uFEFFpackage main\n")
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics for leading BOM: %+v", result.Diagnostics)
	}
	if len(result.Tokens) == 0 || result.Tokens[0].Kind != token.Package {
		t.Fatalf("expected leading BOM to be skipped before package token, got %#v", result.Tokens)
	}

	for _, tc := range []struct {
		name string
		src  string
		code string
	}{
		{name: "middle BOM", src: "package \uFEFFmain", code: "scanner.bom.invalid"},
		{name: "NUL", src: "package main\nvar x = \x00\n", code: "scanner.nul.invalid"},
		{name: "NUL in raw string", src: "package main\nvar x = `\x00`\n", code: "scanner.nul.invalid"},
	} {
		result := Scan(tc.name+".mgo", tc.src)
		if !hasDiagnostic(result.Diagnostics, tc.code) {
			t.Fatalf("%s: expected diagnostic %s, got %+v", tc.name, tc.code, result.Diagnostics)
		}
	}
}

func TestScanCommentsAndSemicolonInsertion(t *testing.T) {
	source := "package main // package comment\nvar x = 1 /* block\ncomment */\nvar y = 2"
	result := Scan("comments.mgo", source)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	wantSemicolons := 3
	gotSemicolons := 0
	for _, scanned := range result.Tokens {
		if scanned.Kind == token.Semicolon {
			gotSemicolons++
		}
	}
	if gotSemicolons != wantSemicolons {
		t.Fatalf("semicolon count = %d, want %d; kinds=%#v", gotSemicolons, wantSemicolons, kinds(result.Tokens))
	}
}

func TestScanPreservesRawSourceElements(t *testing.T) {
	source := "\ufeffpackage main // package\n\n/* block\ncomment */func main() {}\n"
	result := Scan("main.mgo", source)
	var rebuilt strings.Builder
	kinds := map[ElementKind]bool{}
	for _, element := range result.Elements {
		rebuilt.WriteString(element.Lexeme)
		kinds[element.Kind] = true
	}
	if rebuilt.String() != source {
		t.Fatalf("raw elements changed source:\n got %q\nwant %q", rebuilt.String(), source)
	}
	for _, kind := range []ElementKind{ElementBOM, ElementToken, ElementWhitespace, ElementNewline, ElementLineComment, ElementBlockComment} {
		if !kinds[kind] {
			t.Fatalf("raw elements are missing %s: %#v", kind, result.Elements)
		}
	}
}

func TestScanEmitsIllegalTokenForInvalidSourceByte(t *testing.T) {
	result := Scan("main.mgo", "package main\n\x00\xff")
	if !hasKind(result.Tokens, token.Illegal) || len(result.Diagnostics) < 2 {
		t.Fatalf("invalid bytes were not preserved as illegal tokens: %#v %#v", result.Tokens, result.Diagnostics)
	}
}

func TestScanLimitsTokensAndDiagnostics(t *testing.T) {
	file := source.NewFile("file.0", "main.mgo", "@ @ @ @ @")
	result := ScanFileWithLimits(file, Limits{MaxTokens: 2, MaxDiagnostics: 1})
	if len(result.Tokens) > 3 || !hasDiagnostic(result.Diagnostics, string(source.DiagnosticTruncated)) {
		t.Fatalf("scanner limits were not enforced: %#v %#v", result.Tokens, result.Diagnostics)
	}
}

func TestScanLexicalDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{name: "string newline", src: "\"broken\n\"", code: "scanner.string.newline"},
		{name: "rune count", src: "'ab'", code: "scanner.rune.count"},
		{name: "escape", src: "\"\\q\"", code: "scanner.escape.invalid"},
		{name: "short octal escape", src: "\"\\1\"", code: "scanner.escape.octal"},
		{name: "octal escape range", src: "\"\\400\"", code: "scanner.escape.octal_range"},
		{name: "unicode escape range", src: "\"\\U00110000\"", code: "scanner.escape.unicode_range"},
		{name: "unicode surrogate escape", src: "\"\\uD800\"", code: "scanner.escape.unicode_range"},
		{name: "block comment", src: "/* broken", code: "scanner.comment.unterminated"},
		{name: "illegal", src: "@", code: "scanner.token.illegal"},
	}
	for _, tc := range cases {
		result := Scan(tc.name+".mgo", tc.src)
		if !hasDiagnostic(result.Diagnostics, tc.code) {
			t.Fatalf("%s: expected diagnostic %s, got %+v", tc.name, tc.code, result.Diagnostics)
		}
	}
}

func assertKinds(t *testing.T, tokens []Token, want []token.Kind) {
	t.Helper()
	got := kinds(tokens)
	if len(got) != len(want) {
		t.Fatalf("token kind count = %d, want %d\ngot:  %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d = %s, want %s\ngot:  %#v\nwant: %#v", i, got[i], want[i], got, want)
		}
	}
}

func kinds(tokens []Token) []token.Kind {
	out := make([]token.Kind, len(tokens))
	for i, scanned := range tokens {
		out[i] = scanned.Kind
	}
	return out
}

func hasKind(tokens []Token, kind token.Kind) bool {
	for _, scanned := range tokens {
		if scanned.Kind == kind {
			return true
		}
	}
	return false
}

func hasLexeme(tokens []Token, kind token.Kind, lexeme string) bool {
	for _, scanned := range tokens {
		if scanned.Kind == kind && scanned.Lexeme == lexeme {
			return true
		}
	}
	return false
}

func hasDiagnostic(diagnostics []source.Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if string(diagnostic.Code) == code {
			return true
		}
	}
	return false
}
