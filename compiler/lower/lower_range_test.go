package lower

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerRangeStatementToLabelsAndIndex(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"xs"},
							Type:  arrayType,
							Values: []ast.Expression{{
								Kind:  ast.ExprComposite,
								Type:  &arrayType,
								Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}}, {Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}}},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"i"},
							Type:   intType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"v"},
							Type:   intType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
						}}},
					}, {
						Kind:  ast.StmtRange,
						Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "i"}),
						Value: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "v"}),
						Range: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "xs"}),
						Body: ast.BlockStmt{Stmts: []ast.Statement{{
							Kind: ast.StmtBranch,
							Op:   "continue",
						}}},
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
	if countStatements(fn.Body, ir.StmtJumpIf) != 1 {
		t.Fatalf("expected range to lower to one conditional jump, got %#v", fn.Body)
	}
	if countStatements(fn.Body, ir.StmtLabel) < 4 {
		t.Fatalf("expected range labels, got %#v", fn.Body)
	}
	var sawLen, sawIndex bool
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtStoreLocal && stmt.Expr.Kind == ir.ExprLen {
			sawLen = true
		}
		if stmt.Kind == ir.StmtStoreLocal && stmt.Expr.Kind == ir.ExprLoadIndex {
			sawIndex = true
		}
	}
	if !sawLen || !sawIndex {
		t.Fatalf("expected range to use len and index expressions, got %#v", fn.Body)
	}
}

func TestLowerStringRangeUsesRuneAndNextIndexPrimitives(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	runeType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int32"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"s"},
							Type:   stringType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "hello", Type: &stringType}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"i"},
							Type:   intType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"r"},
							Type:   runeType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &runeType}},
						}}},
					}, {
						Kind:  ast.StmtRange,
						Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "i"}),
						Value: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "r"}),
						Range: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "s"}),
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
	var sawRuneAt, sawNextIndex bool
	for _, stmt := range fn.Body {
		sawRuneAt = sawRuneAt || stmt.Kind == ir.StmtStoreLocal && stmt.Expr.Kind == ir.ExprStringRuneAt
		sawNextIndex = sawNextIndex || stmt.Kind == ir.StmtStoreLocal && strings.HasPrefix(stmt.Local, "local.range.index") && stmt.Expr.Kind == ir.ExprStringNextRuneIndex
	}
	if !sawRuneAt || !sawNextIndex {
		t.Fatalf("expected string range to use rune-at and next-index primitives, got %#v", fn.Body)
	}
}

func TestLowerMapRangeOwnsEntryIterator(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	mapType := ast.TypeExpr{Kind: ast.TypeMap, Key: &stringType, Elem: &intType}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"m"},
							Type:  mapType,
							Values: []ast.Expression{{
								Kind: ast.ExprComposite,
								Type: &mapType,
								Items: []ast.KeyValue{{
									Key:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "a", Type: &stringType}),
									Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
								}},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"k"},
							Type:   stringType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "init", Type: &stringType}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names:  []string{"v"},
							Type:   intType,
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
						}}},
					}, {
						Kind:  ast.StmtRange,
						Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "k"}),
						Value: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "v"}),
						Range: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "m"}),
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
	var initialized, advanced, closed bool
	for _, stmt := range fn.Body {
		initialized = initialized || stmt.Kind == ir.StmtMapIterInit
		advanced = advanced || stmt.Kind == ir.StmtStoreResults && stmt.Expr.Kind == ir.ExprMapIterNext && len(stmt.Targets) == 3
		closed = closed || stmt.Kind == ir.StmtMapIterClose
	}
	if !initialized || !advanced || !closed {
		t.Fatalf("map iteration must own and release an entry cursor and bind key/value/ok")
	}
}

func TestLowerMapIndexCommaOKToLowLevelExpression(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	mapType := ast.TypeExpr{Kind: ast.TypeMap, Key: &stringType, Elem: &intType}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"m"},
							Type:  mapType,
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"v", "ok"},
							Values: []ast.Expression{{
								Kind:    ast.ExprIndex,
								Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "m"}),
								Index:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "missing", Type: &stringType}),
							}},
						}}},
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
	localTypes := map[string]string{}
	for _, local := range fn.Locals {
		localTypes[local.ID] = hirTypeString(&program.TypeTable, local.Type)
	}
	if localTypes["local.v"] != "Int64" || localTypes["local.ok"] != "Bool" {
		t.Fatalf("expected comma-ok local types, got %#v", fn.Locals)
	}
	var sawMapIndexOK bool
	for _, stmt := range fn.Body {
		sawMapIndexOK = sawMapIndexOK || stmt.Kind == ir.StmtStoreResults &&
			len(stmt.Targets) == 2 && stmt.Expr.Kind == ir.ExprLoadIndexOK
	}
	if !sawMapIndexOK {
		t.Fatalf("expected map comma-ok to lower to load_index_ok, got %#v", fn.Body)
	}
}
