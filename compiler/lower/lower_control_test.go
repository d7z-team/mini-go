package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/parser"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func TestLowerBranchDeferGoAndPanicStatements(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	program, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{Name: "Cleanup"},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:   "Worker",
					Params: []ast.Field{{Name: "x", Type: intType}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Fail",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtPanic,
						Expr: ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "failed", Type: &ast.TypeExpr{Kind: ast.TypeName, Name: "String"}}),
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDefer,
						Expr: ptrExpr(ast.Expression{
							Kind:   ast.ExprCall,
							Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Cleanup"}),
						}),
					}, {
						Kind: ast.StmtGo,
						Expr: ptrExpr(ast.Expression{
							Kind:   ast.ExprCall,
							Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Worker"}),
							Args:   []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: &intType}},
						}),
					}, {
						Kind: ast.StmtFor,
						Cond: ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "true", Type: &boolType}),
						Body: ast.BlockStmt{Stmts: []ast.Statement{{
							Kind: ast.StmtBranch,
							Op:   "continue",
						}, {
							Kind: ast.StmtBranch,
							Op:   "break",
						}}},
					}}},
				},
			}},
		}},
	}, Options{Dependencies: testDependencies([]check.DependencyExport{{
		ModulePath: "example/lib",
		Name:       "Add",
		Kind:       check.ObjectFunc,
		Type:       "function(Int64, Int64) Int64",
	}})})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if countStatements(mainFn.Body, ir.StmtDefer) != 1 {
		t.Fatalf("expected one defer statement, got %#v", mainFn.Body)
	}
	if countStatements(mainFn.Body, ir.StmtSpawn) != 1 {
		t.Fatalf("expected one spawn statement, got %#v", mainFn.Body)
	}
	if countStatements(mainFn.Body, ir.StmtJump) < 2 {
		t.Fatalf("expected branch jumps, got %#v", mainFn.Body)
	}
	failFn, ok := findFunction(program.Functions, "fn.Fail")
	if !ok || countStatements(failFn.Body, ir.StmtPanic) != 1 {
		t.Fatalf("expected Fail panic function, got %#v", program.Functions)
	}
}

func TestLowerGoAndDeferBuiltinsThroughCapturedWrappers(t *testing.T) {
	parsed := parser.ParseSource("example/main", "builtins.mgo", `package main

func Main(m map[int]int, ch chan int, values []int) {
	defer delete(m, 1)
	defer clear(values)
	defer close(ch)
	defer copy(values, values)
	defer panic("deferred")
	defer recover()
	go delete(m, 2)
	go clear(values)
	go close(ch)
	go copy(values, values)
	go panic("spawned")
	go recover()
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	program, diagnostics := lowerTestProgram(parsed.Program)
	if len(diagnostics) != 0 {
		t.Fatalf("lower source: %#v", diagnostics)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok || countStatements(mainFn.Body, ir.StmtDefer) != 6 || countStatements(mainFn.Body, ir.StmtSpawn) != 6 {
		t.Fatalf("unexpected Main async statements: %#v", mainFn.Body)
	}
	seen := map[ir.ExpressionKind]bool{}
	panicWrappers := 0
	for _, function := range program.Functions {
		if len(function.Body) != 1 {
			continue
		}
		statement := function.Body[0]
		if statement.Kind == ir.StmtPanic {
			panicWrappers++
		}
		seen[statement.Expr.Kind] = true
	}
	for _, kind := range []ir.ExpressionKind{ir.ExprDelete, ir.ExprClear, ir.ExprChanClose, ir.ExprCopy, ir.ExprRecover} {
		if !seen[kind] {
			t.Fatalf("missing wrapper expression %q in %#v", kind, program.Functions)
		}
	}
	if panicWrappers != 2 {
		t.Fatalf("panic wrapper count = %d, want 2", panicWrappers)
	}
}

func TestLowerRejectsNonCallDeferStatement(t *testing.T) {
	_, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDefer,
						Expr: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Main"}),
					}}},
				},
			}},
		}},
	})
	requireDiagnostic(t, diagnostics, "hirgen.defer.call")
}
