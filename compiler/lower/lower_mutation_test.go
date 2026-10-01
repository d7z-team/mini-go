package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerCompoundAssignment(t *testing.T) {
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
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names:  []string{"x"},
								Type:   intType,
								Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}},
							},
						}},
					}, {
						Kind:  ast.StmtAssign,
						Op:    "+=",
						Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
						Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
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
	if len(fn.Body) < 2 || fn.Body[1].Kind != ir.StmtStoreLocal {
		t.Fatalf("expected compound assignment to lower to local store, got %#v", fn.Body)
	}
	value := fn.Body[1].Expr
	if value.Kind != ir.ExprBinary || value.Operator != "+" {
		t.Fatalf("expected += to lower to + binary expression, got %#v", value)
	}
	if value.Left == nil || value.Left.Kind != ir.ExprLocal || value.Left.Local != "local.x" {
		t.Fatalf("expected compound assignment to read x, got %#v", value.Left)
	}
}

func TestLowerBitClearAssignment(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Uint8"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names:  []string{"bits"},
								Type:   intType,
								Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "7", Type: &intType}},
							},
						}},
					}, {
						Kind:  ast.StmtAssign,
						Op:    "&^=",
						Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "bits"}},
						Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "bits"}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok || len(fn.Body) < 2 {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	value := fn.Body[1].Expr
	if value.Kind != ir.ExprBinary || value.Operator != "&^" {
		t.Fatalf("expected &^= to lower to &^ binary expression, got %#v", value)
	}
}

func TestLowerIncDecAssignment(t *testing.T) {
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
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names:  []string{"x"},
								Type:   intType,
								Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}},
							},
						}},
					}, {
						Kind: ast.StmtAssign,
						Op:   "++",
						Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
					}, {
						Kind: ast.StmtAssign,
						Op:   "--",
						Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
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
	if len(fn.Body) < 3 || fn.Body[1].Kind != ir.StmtStoreLocal || fn.Body[2].Kind != ir.StmtStoreLocal {
		t.Fatalf("expected inc/dec to lower to local stores, got %#v", fn.Body)
	}
	inc := fn.Body[1].Expr
	if inc.Kind != ir.ExprBinary || inc.Operator != "+" {
		t.Fatalf("expected ++ to lower to + binary expression, got %#v", inc)
	}
	dec := fn.Body[2].Expr
	if dec.Kind != ir.ExprBinary || dec.Operator != "-" {
		t.Fatalf("expected -- to lower to - binary expression, got %#v", dec)
	}
}

func TestLowerIncDecIndexAndMemberAssignment(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	structType := ast.TypeExpr{
		Kind: ast.TypeStruct,
		Fields: []ast.Field{{
			Name: "Value",
			Type: intType,
		}},
	}
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
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"arr"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &arrayType,
									Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"box"},
								Type:  structType,
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &structType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Value"}),
										Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "40", Type: &intType},
									}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtAssign,
						Op:   "++",
						Left: []ast.Expression{{
							Kind:    ast.ExprIndex,
							Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "arr"}),
							Index:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}),
						}},
					}, {
						Kind: ast.StmtAssign,
						Op:   "--",
						Left: []ast.Expression{{
							Kind:    ast.ExprSelector,
							Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "box"}),
							Field:   "Value",
						}},
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
	if countStatements(fn.Body, ir.StmtStoreIndirect) != 2 {
		t.Fatalf("expected address-path stores for index and member inc/dec, got %#v", fn.Body)
	}
	if !hasAddressPath(fn.Body, "index") || !hasAddressPath(fn.Body, "field") {
		t.Fatalf("expected index and member address paths, got %#v", fn.Body)
	}
}

func TestLowerCompoundIndexAndMemberAssignment(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	structType := ast.TypeExpr{
		Kind: ast.TypeStruct,
		Fields: []ast.Field{{
			Name: "Value",
			Type: intType,
		}},
	}
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
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"arr"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &arrayType,
									Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"box"},
								Type:  structType,
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &structType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Value"}),
										Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "40", Type: &intType},
									}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtAssign,
						Op:   "+=",
						Left: []ast.Expression{{
							Kind:    ast.ExprIndex,
							Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "arr"}),
							Index:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}),
						}},
						Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}},
					}, {
						Kind: ast.StmtAssign,
						Op:   "+=",
						Left: []ast.Expression{{
							Kind:    ast.ExprSelector,
							Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "box"}),
							Field:   "Value",
						}},
						Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}},
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
	if countStatements(fn.Body, ir.StmtStoreIndirect) != 2 {
		t.Fatalf("expected address-path stores for index and member compound targets, got %#v", fn.Body)
	}
	if !hasAddressPath(fn.Body, "index") || !hasAddressPath(fn.Body, "field") {
		t.Fatalf("expected index and member address paths, got %#v", fn.Body)
	}
}
