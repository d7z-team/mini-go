package parser

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func TestBracketExpressionsPreserveBoundsAndSourceSpan(t *testing.T) {
	for _, test := range []struct {
		text            string
		kind            ast.ExprKind
		start, end, max string
	}{
		{"x[:]", ast.ExprSlice, "", "", ""},
		{"x[:hi]", ast.ExprSlice, "", "hi", ""},
		{"x[lo:]", ast.ExprSlice, "lo", "", ""},
		{"x[lo:hi]", ast.ExprSlice, "lo", "hi", ""},
		{"x[:hi:cap]", ast.ExprSlice, "", "hi", "cap"},
		{"x[lo:hi:cap]", ast.ExprSlice, "lo", "hi", "cap"},
		{"x[i]", ast.ExprIndex, "", "", ""},
		{"x[A,B]", ast.ExprIndexList, "", "", ""},
	} {
		t.Run(test.text, func(t *testing.T) {
			text := "package sample\nvar v = " + test.text
			result := ParseSource("sample", "main.mgo", text)
			requireNoDiagnostics(t, result)
			expr := result.Program.Files[0].Decls[0].Var.Values[0]
			if expr.Kind != test.kind || expr.Operand == nil || expr.Operand.Name != "x" {
				t.Fatalf("bracket expression = %+v", expr)
			}
			for _, bound := range []struct {
				value *ast.Expression
				name  string
			}{{expr.Start, test.start}, {expr.End, test.end}, {expr.Max, test.max}} {
				if bound.name == "" {
					if bound.value != nil {
						t.Fatalf("unexpected bound: %+v", bound.value)
					}
				} else if bound.value == nil || bound.value.Name != bound.name {
					t.Fatalf("bound = %+v, want %s", bound.value, bound.name)
				}
			}
			if test.kind == ast.ExprIndex && (expr.Index == nil || expr.Index.Name != "i") {
				t.Fatalf("index = %+v", expr.Index)
			}
			if test.kind == ast.ExprIndexList && (len(expr.Args) != 2 || expr.Args[0].Name != "A" || expr.Args[1].Name != "B") {
				t.Fatalf("type arguments = %+v", expr.Args)
			}
			if got := text[expr.Span.Start.Offset:expr.Span.End.Offset]; got != test.text {
				t.Fatalf("source span = %q", got)
			}
		})
	}
}

func TestSliceMissingBracketReportsSliceDiagnostic(t *testing.T) {
	for _, expression := range []string{"x[:hi", "x[lo:hi", "x[:hi:cap", "x[lo:hi:cap"} {
		result := ParseSource("sample", "main.mgo", "package sample\nvar v = "+expression)
		found := false
		for _, diagnostic := range result.Diagnostics {
			found = found || diagnostic.Code == "parser.slice"
		}
		if !found {
			t.Fatalf("%s: %+v", expression, result.Diagnostics)
		}
	}
}

func BenchmarkBracketExpressions(b *testing.B) {
	text := "package sample\nfunc F(x []int, lo, hi, cap int) { _ = x[:]; _ = x[:hi]; _ = x[lo:]; _ = x[lo:hi]; _ = x[:hi:cap]; _ = x[lo:hi:cap]; _ = x[lo] }"
	b.ReportAllocs()
	for b.Loop() {
		if result := ParseSource("sample", "main.mgo", text); len(result.Diagnostics) != 0 {
			b.Fatal(result.Diagnostics)
		}
	}
}
