package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerAddressOfCompositeLiteral(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	counterStruct := ast.TypeExpr{Kind: ast.TypeStruct, Fields: []ast.Field{{Name: "Value", Type: intType}}}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Counter", Type: counterStruct},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"p"},
								Values: []ast.Expression{{
									Kind: ast.ExprAddr,
									Operand: ptrExpr(ast.Expression{
										Kind: ast.ExprComposite,
										Type: &counterType,
										Items: []ast.KeyValue{{
											Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Value"}),
											Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "40", Type: &intType},
										}},
									}),
								}},
							},
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if len(mainFn.Body) == 0 || mainFn.Body[0].Expr.Kind != ir.ExprLet {
		t.Fatalf("expected address-of composite to lower through let temp, got %#v", mainFn.Body)
	}
	let := mainFn.Body[0].Expr
	if let.Bind == nil || let.Bind.Kind != ir.ExprStruct || let.Body == nil || let.Body.Kind != ir.ExprAddressOf || let.Body.Local != let.Local {
		t.Fatalf("unexpected lowered address-of composite expression: %#v", let)
	}
	var tempType string
	for _, local := range mainFn.Locals {
		if local.ID == let.Local {
			tempType = hirTypeString(&program.TypeTable, local.Type)
			break
		}
	}
	if tempType != "example/main.Counter" {
		t.Fatalf("expected temp local to preserve composite type Counter, got %q", tempType)
	}
}

func TestLowerMultiResultAssignment(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Pair",
					Results: []ast.Field{{Type: intType}, {Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}, {Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}, {Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"a", "b"},
								Type:  intType,
							},
						}},
					}, {
						Kind: ast.StmtAssign,
						Left: []ast.Expression{
							{Kind: ast.ExprIdent, Name: "a"},
							{Kind: ast.ExprIdent, Name: "b"},
						},
						Right: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: &ast.Expression{Kind: ast.ExprIdent, Name: "Pair"},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{
							{Kind: ast.ExprIdent, Name: "a"},
							{Kind: ast.ExprIdent, Name: "b"},
						},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if len(fn.Body) < 4 || fn.Body[0].Kind != ir.StmtStoreLocal || !fn.Body[0].Rebind ||
		fn.Body[1].Kind != ir.StmtStoreLocal || !fn.Body[1].Rebind {
		t.Fatalf("expected zero-valued declaration rebinds before assignment, got %#v", fn.Body)
	}
	store := fn.Body[2]
	if store.Kind != ir.StmtStoreResults {
		t.Fatalf("expected multi-result assignment after declarations, got %#v", fn.Body)
	}
	if store.Expr.Kind != ir.ExprCallDirect || store.Expr.ResultCount != 2 {
		t.Fatalf("expected multi-result direct call expression, got %#v", store.Expr)
	}
	if len(store.Targets) != 2 || store.Targets[0].Local != "local.a" || store.Targets[1].Local != "local.b" {
		t.Fatalf("expected a,b store targets, got %#v", store.Targets)
	}
}

func TestLowerMultiResultVarDeclaration(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Pair",
					Results: []ast.Field{{Type: intType}, {Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}, {Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}, {Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"a", "b"},
								Type:  intType,
								Values: []ast.Expression{{
									Kind:   ast.ExprCall,
									Callee: &ast.Expression{Kind: ast.ExprIdent, Name: "Pair"},
								}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{
							{Kind: ast.ExprIdent, Name: "a"},
							{Kind: ast.ExprIdent, Name: "b"},
						},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if len(fn.Body) < 1 || fn.Body[0].Kind != ir.StmtStoreResults {
		t.Fatalf("expected var declaration to lower to store_results, got %#v", fn.Body)
	}
	store := fn.Body[0]
	if store.Expr.Kind != ir.ExprCallDirect || store.Expr.ResultCount != 2 {
		t.Fatalf("expected multi-result direct call initializer, got %#v", store.Expr)
	}
	if len(store.Targets) != 2 || store.Targets[0].Local != "local.a" || store.Targets[1].Local != "local.b" {
		t.Fatalf("expected a,b store targets, got %#v", store.Targets)
	}
}

func TestLowerMultiValueAssignment(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}, {Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"a", "b"},
								Type:  intType,
								Values: []ast.Expression{
									{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
									{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
								},
							},
						}},
					}, {
						Kind: ast.StmtAssign,
						Left: []ast.Expression{
							{Kind: ast.ExprIdent, Name: "a"},
							{Kind: ast.ExprIdent, Name: "b"},
						},
						Right: []ast.Expression{
							{Kind: ast.ExprIdent, Name: "b"},
							{Kind: ast.ExprIdent, Name: "a"},
						},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{
							{Kind: ast.ExprIdent, Name: "a"},
							{Kind: ast.ExprIdent, Name: "b"},
						},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if len(fn.Body) < 2 || fn.Body[1].Kind != ir.StmtStoreValues {
		t.Fatalf("expected assignment to lower to store_values, got %#v", fn.Body)
	}
	store := fn.Body[1]
	if len(store.Values) != 2 || store.Values[0].Local != "local.b" || store.Values[1].Local != "local.a" {
		t.Fatalf("expected RHS values b,a, got %#v", store.Values)
	}
	if len(store.Targets) != 2 || store.Targets[0].Local != "local.a" || store.Targets[1].Local != "local.b" {
		t.Fatalf("expected a,b store targets, got %#v", store.Targets)
	}
}
