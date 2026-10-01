package semantic

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
)

func TestStructuralFailurePrecedesRecursiveSemanticAnalysis(t *testing.T) {
	cycle := &ast.Expression{Kind: ast.ExprUnary, Operator: "!"}
	cycle.Operand = cycle
	program := ast.Program{ModulePath: "sample", Package: "sample", Files: []ast.File{{Path: "main.mgo", Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{Names: []string{"value"}, Values: []ast.Expression{*cycle}}}}}}}
	checked := WithOptions(program, AnalyzeOptions{})
	for _, diagnostic := range checked.Info.Diagnostics {
		if diagnostic.Code == "ast.pointer.cycle" {
			return
		}
	}
	t.Fatalf("expected bounded structural failure, got %v", checked.Info.Diagnostics)
}

func TestOwnedPackageAnalysisMatchesExternalASTFacts(t *testing.T) {
	files := []source.File{
		source.NewFile("a", "a.mgo", "package sample\ntype Pair struct { Left, Right int }\nconst Base = 21\n"),
		source.NewFile("b", "b.mgo", "package sample\nfunc Run() int { p := Pair{Left: Base, Right: Base}; return p.Left + p.Right }\n"),
	}
	parsed, _, diagnostics := parser.NewPackageBuilder("sample", files, parser.Limits{}).Finalize()
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	checked, documents := WithParsed(parsed, AnalyzeOptions{})
	if len(checked.Info.Diagnostics) != 0 || len(documents) != len(files) {
		t.Fatalf("analysis: %v, documents=%d", checked.Info.Diagnostics, len(documents))
	}
	external := WithOptions(ast.CloneProgram(checked.Program), AnalyzeOptions{})
	if len(external.Info.Diagnostics) != 0 || !reflect.DeepEqual(checked.Program, external.Program) ||
		!reflect.DeepEqual(checked.Info.Constants, external.Info.Constants) || !reflect.DeepEqual(checked.Info.Exprs, external.Info.Exprs) ||
		!reflect.DeepEqual(checked.Info.NameDefs, external.Info.NameDefs) || !reflect.DeepEqual(checked.Info.NameUses, external.Info.NameUses) {
		t.Fatalf("owned and external facts differ: diagnostics=%v", external.Info.Diagnostics)
	}
	checkedAgain, _ := WithParsed(parsed, AnalyzeOptions{})
	if len(checkedAgain.Info.Diagnostics) != 1 || checkedAgain.Info.Diagnostics[0].Code != "ast.program.missing" {
		t.Fatalf("consumed syntax reused: %v", checkedAgain.Info.Diagnostics)
	}
}

func TestSharedTreeBudgetPrecedesRecursiveSemanticAnalysis(t *testing.T) {
	node := &ast.Expression{Kind: ast.ExprLiteral, Literal: "1"}
	for range 32 {
		node = &ast.Expression{Kind: ast.ExprBinary, Operator: "+", Left: node, Right: node}
	}
	program := ast.Program{Files: []ast.File{{Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{Values: []ast.Expression{*node}}}}}}}
	checked := WithOptions(program, AnalyzeOptions{Limits: ast.Limits{MaxNodes: 128}})
	for _, diagnostic := range checked.Info.Diagnostics {
		if diagnostic.Code == "ast.limit.nodes" {
			return
		}
	}
	t.Fatalf("shared subtree bypassed semantic work limit: %v", checked.Info.Diagnostics)
}
