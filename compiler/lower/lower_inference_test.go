package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func TestLowerInfersLocalTypeFromCompositeInitializer(t *testing.T) {
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
						Kind:  ast.StmtRange,
						Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "k"}),
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
	var sawInferredMapLocal bool
	for _, local := range fn.Locals {
		sawInferredMapLocal = sawInferredMapLocal || local.ID == "local.m" && hirTypeString(&program.TypeTable, local.Type) == "Map<String, Int64>"
	}
	if !sawInferredMapLocal {
		t.Fatalf("expected inferred map local, locals=%#v", fn.Locals)
	}
}

func TestLowerFunctionValueParameterCallInfersResultType(t *testing.T) {
	runeType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int32"}
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	mapperType := ast.TypeExpr{
		Kind: ast.TypeFunc,
		Params: []ast.Field{{
			Name: "value",
			Type: runeType,
		}},
		Results: []ast.Field{{Type: runeType}},
	}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Keep",
					Params:  []ast.Field{{Name: "mapping", Type: mapperType}},
					Results: []ast.Field{{Type: boolType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"mapped"},
								Values: []ast.Expression{{
									Kind:   ast.ExprCall,
									Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "mapping"}),
									Args: []ast.Expression{{
										Kind:    ast.ExprLiteral,
										Literal: "97",
										Type:    &runeType,
									}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:     ast.ExprBinary,
							Operator: ">=",
							Left:     ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "mapped"}),
							Right: ptrExpr(ast.Expression{
								Kind:    ast.ExprLiteral,
								Literal: "0",
								Type:    &runeType,
							}),
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Keep")
	if !ok {
		t.Fatalf("expected Keep function, got %#v", program.Functions)
	}
	var sawMappedRune bool
	for _, local := range fn.Locals {
		sawMappedRune = sawMappedRune || local.ID == "local.mapped" && hirTypeString(&program.TypeTable, local.Type) == "Int32"
	}
	if !sawMappedRune {
		t.Fatalf("expected function value call result local to be Int32, locals=%#v", fn.Locals)
	}
}
