package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerBuiltinLenCapCalls(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
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
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "len"}),
							Args: []ast.Expression{{
								Kind:    ast.ExprLiteral,
								Literal: `"abc"`,
								Type:    &stringType,
							}},
						}, {
							Kind:   ast.ExprCall,
							Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "cap"}),
							Args: []ast.Expression{{
								Kind:  ast.ExprComposite,
								Type:  &arrayType,
								Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}}, {Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}}},
							}},
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
	if len(fn.Body) == 0 || len(fn.Body[0].Results) != 2 {
		t.Fatalf("expected two return expressions, got %#v", fn.Body)
	}
	if got := fn.Body[0].Results[0]; got.Kind != ir.ExprLiteral || hirTypeString(&program.TypeTable, got.Type) != "Int" || string(got.Value) != "3" {
		t.Fatalf("expected len builtin to fold to Int literal 3, got %#v", got)
	}
	if got := fn.Body[0].Results[1].Kind; got != ir.ExprCap {
		t.Fatalf("expected cap builtin to lower to ExprCap, got %s", got)
	}
}

func TestLowerBuiltinAppendDeleteCalls(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	mapType := ast.TypeExpr{Kind: ast.TypeMap, Key: &intType, Elem: &intType}
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
								Names: []string{"xs"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &arrayType,
									Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"m"},
								Type:  mapType,
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &mapType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}),
										Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
									}},
								}},
							},
						}},
					}, {
						Kind:  ast.StmtAssign,
						Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "xs"}},
						Right: []ast.Expression{{Kind: ast.ExprCall, Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "append"}), Args: []ast.Expression{{Kind: ast.ExprIdent, Name: "xs"}, {Kind: ast.ExprLiteral, Literal: "2", Type: &intType}}}},
					}, {
						Kind: ast.StmtExpr,
						Expr: ptrExpr(ast.Expression{Kind: ast.ExprCall, Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "delete"}), Args: []ast.Expression{{Kind: ast.ExprIdent, Name: "m"}, {Kind: ast.ExprLiteral, Literal: "1", Type: &intType}}}),
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
	if len(fn.Body) < 4 || fn.Body[2].Expr.Kind != ir.ExprAppend {
		t.Fatalf("expected append assignment expression, got %#v", fn.Body)
	}
	if fn.Body[3].Expr.Kind != ir.ExprDelete {
		t.Fatalf("expected delete expression statement, got %#v", fn.Body[3])
	}
}

func TestLowerBuiltinAppendEllipsisCall(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
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
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"ys"},
							Type:  arrayType,
						}}},
					}, {
						Kind: ast.StmtAssign,
						Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "xs"}},
						Right: []ast.Expression{{
							Kind:     ast.ExprCall,
							Callee:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "append"}),
							Args:     []ast.Expression{{Kind: ast.ExprIdent, Name: "xs"}, {Kind: ast.ExprIdent, Name: "ys"}},
							Ellipsis: true,
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
	if len(fn.Body) != 3 || fn.Body[0].Kind != ir.StmtStoreLocal || !fn.Body[0].Rebind ||
		fn.Body[1].Kind != ir.StmtStoreLocal || !fn.Body[1].Rebind ||
		fn.Body[2].Expr.Kind != ir.ExprAppend || !fn.Body[2].Expr.Ellipsis || len(fn.Body[2].Expr.Args) != 1 {
		t.Fatalf("expected append ellipsis expression, got %#v", fn.Body)
	}
}

func TestLowerBuiltinClearCopyCalls(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	mapType := ast.TypeExpr{Kind: ast.TypeMap, Key: &intType, Elem: &intType}
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
								Names: []string{"xs"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &arrayType,
									Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"m"},
								Type:  mapType,
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &mapType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}),
										Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
									}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtExpr,
						Expr: ptrExpr(ast.Expression{Kind: ast.ExprCall, Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "clear"}), Args: []ast.Expression{{Kind: ast.ExprIdent, Name: "m"}}}),
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"n"},
								Type:  intType,
								Values: []ast.Expression{{
									Kind:   ast.ExprCall,
									Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "copy"}),
									Args:   []ast.Expression{{Kind: ast.ExprIdent, Name: "xs"}, {Kind: ast.ExprIdent, Name: "xs"}},
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
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if len(fn.Body) < 4 || fn.Body[2].Expr.Kind != ir.ExprClear {
		t.Fatalf("expected clear expression statement, got %#v", fn.Body)
	}
	if fn.Body[3].Kind != ir.StmtStoreResults || fn.Body[3].Expr.Kind != ir.ExprCopy {
		t.Fatalf("expected copy initializer to lower to store_results, got %#v", fn.Body[3])
	}
}

func TestLowerBuiltinInitializerTypes(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
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
							Values: []ast.Expression{{
								Kind:  ast.ExprComposite,
								Type:  &arrayType,
								Items: []ast.KeyValue{{Value: ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType}}},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"ys"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "append"}),
								Args: []ast.Expression{
									{Kind: ast.ExprIdent, Name: "xs"},
									{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
								},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"n"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "copy"}),
								Args:   []ast.Expression{{Kind: ast.ExprIdent, Name: "ys"}, {Kind: ast.ExprIdent, Name: "xs"}},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"l"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "len"}),
								Args:   []ast.Expression{{Kind: ast.ExprIdent, Name: "ys"}},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"c"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "cap"}),
								Args:   []ast.Expression{{Kind: ast.ExprIdent, Name: "ys"}},
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
	typesByName := map[string]string{}
	for _, local := range fn.Locals {
		typesByName[local.Name] = hirTypeString(&program.TypeTable, local.Type)
	}
	if typesByName["ys"] != "Slice<Int64>" {
		t.Fatalf("expected append initializer to infer Slice<Int64>, got locals %#v", fn.Locals)
	}
	for _, name := range []string{"n", "l", "c"} {
		if typesByName[name] != "Int" {
			t.Fatalf("expected %s initializer to infer Int, got locals %#v", name, fn.Locals)
		}
	}
}

func TestLowerVoidBuiltinInitializerReportsSemanticNoValue(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	mapType := ast.TypeExpr{Kind: ast.TypeMap, Key: &intType, Elem: &intType}
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
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"m"},
							Type:  mapType,
							Values: []ast.Expression{{
								Kind: ast.ExprComposite,
								Type: &mapType,
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"x"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "delete"}),
								Args: []ast.Expression{
									{Kind: ast.ExprIdent, Name: "m"},
									{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
								},
							}},
						}}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) == 0 {
		t.Fatalf("expected result mismatch diagnostic for delete initializer")
	}
	if diagnostics[len(diagnostics)-1].Code != "semantic.decl.no_value" {
		t.Fatalf("expected semantic.decl.no_value diagnostic, got %#v", diagnostics)
	}
}

func TestLowerBuiltinMakeSliceAndMapExpressions(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
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
							Names: []string{"xs"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "make"}),
								Args: []ast.Expression{{
									Kind: ast.ExprIdent,
									Name: "type",
									Type: &arrayType,
								}, {
									Kind:    ast.ExprLiteral,
									Literal: "2",
									Type:    &intType,
								}},
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"m"},
							Values: []ast.Expression{{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "make"}),
								Args: []ast.Expression{{
									Kind: ast.ExprIdent,
									Name: "type",
									Type: &mapType,
								}},
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
	if len(fn.Body) < 2 || fn.Body[0].Expr.Kind != ir.ExprMakeSlice || fn.Body[1].Expr.Kind != ir.ExprMap {
		t.Fatalf("expected make slice and map expressions, got %#v", fn.Body)
	}
	if hirTypeString(&program.TypeTable, fn.Body[0].Expr.Type) != "Slice<Int64>" || hirTypeString(&program.TypeTable, fn.Body[1].Expr.Type) != "Map<String, Int64>" {
		t.Fatalf("unexpected make expression types: %#v %#v", fn.Body[0].Expr, fn.Body[1].Expr)
	}
}
