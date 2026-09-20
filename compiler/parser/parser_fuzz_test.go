package parser_test

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
)

func FuzzParseTerminatesWithBoundedDiagnostics(f *testing.F) {
	f.Add("package main\nfunc main() {}\n")
	f.Add("package main\nfunc broken( {\n")
	f.Add("package main\nfunc make(value int) int { return value }; func Main() { _ = make(1) }\n")
	f.Add("package main\nfunc F[T int | ~int](value T) {}\n")
	f.Add("########0A#0A##0A!####0A#0A#0A###0A#0A#0A##0A##0A##0A##0A\"")
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 1<<20 {
			t.Skip()
		}
		result := parser.ParseSource("fuzz/module", "fuzz.mgo", source)
		if len(result.Diagnostics) > parser.DefaultMaxDiagnostics+1 {
			t.Fatalf("diagnostic budget exceeded: %d", len(result.Diagnostics))
		}
		again := parser.ParseSource("fuzz/module", "fuzz.mgo", source)
		if !reflect.DeepEqual(result, again) {
			t.Fatal("parser output is not deterministic")
		}
		if diagnostics := ast.ValidateStructure(&result.Program, ast.Limits{}); len(diagnostics) != 0 {
			t.Fatalf("parser returned structurally invalid AST: %#v", diagnostics)
		}
	})
}

func TestParseAppliesASTNodeLimit(t *testing.T) {
	result := parser.ParseFileWithLimits("limits/module", source.NewFile("limits", "limits.mgo", "package main\nfunc main() { var a, b, c int }\n"), parser.Limits{MaxASTNodes: 4})
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "ast.limit.nodes" {
			return
		}
	}
	t.Fatalf("missing AST node limit diagnostic: %#v", result.Diagnostics)
}
