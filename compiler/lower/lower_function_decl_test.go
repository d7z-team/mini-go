package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerNilLiteral(t *testing.T) {
	anyType := ast.TypeExpr{Kind: ast.TypeName, Name: "Any"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: anyType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:    ast.ExprLiteral,
							Literal: "nil",
							Type:    &anyType,
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
	literal := fn.Body[0].Results[0]
	if literal.Kind != ir.ExprLiteral || hirTypeString(&program.TypeTable, literal.Type) != "Any" || string(literal.Value) != "null" {
		t.Fatalf("expected nil literal to lower as Any null, got %#v", literal)
	}
}

func TestLowerFunctionLiteralCapturesLocal(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	fnExpr := ast.Expression{
		Kind: ast.ExprFunc,
		Func: &ast.FuncDecl{
			Results: []ast.Field{{Type: intType}},
			Body: ast.BlockStmt{Stmts: []ast.Statement{{
				Kind: ast.StmtAssign,
				Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
				Right: []ast.Expression{{
					Kind:     ast.ExprBinary,
					Operator: "+",
					Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "x"},
					Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
				}},
			}, {
				Kind:    ast.StmtReturn,
				Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
			}}},
		},
	}
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
								Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "41", Type: &intType}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: &fnExpr,
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	literal, ok := findFunction(program.Functions, "fn.literal.1")
	if !ok {
		t.Fatalf("expected function literal, got %#v", program.Functions)
	}
	if !literal.RevisionLocal {
		t.Fatal("function literal must be revision-local")
	}
	if len(literal.Upvalues) != 1 || literal.Upvalues[0].ID != "up.x" || hirTypeString(&program.TypeTable, literal.Upvalues[0].Type) != "Int64" {
		t.Fatalf("expected x upvalue, got %#v", literal.Upvalues)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if mainFn.RevisionLocal {
		t.Fatal("declared function must have logical revision identity")
	}
	call := mainFn.Body[1].Results[0]
	if call.Kind != ir.ExprCallValue || call.Operand == nil || call.Operand.Kind != ir.ExprFunction {
		t.Fatalf("expected call of function literal, got %#v", call)
	}
	if len(call.Operand.Captures) != 1 || call.Operand.Captures[0].Local != "local.x" {
		t.Fatalf("expected capture of local.x, got %#v", call.Operand.Captures)
	}
}

func TestLowerFunctionLiteralLocalDeclType(t *testing.T) {
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
								Names: []string{"next"},
								Values: []ast.Expression{{
									Kind: ast.ExprFunc,
									Func: &ast.FuncDecl{
										Results: []ast.Field{{Type: intType}},
										Body: ast.BlockStmt{Stmts: []ast.Statement{{
											Kind:    ast.StmtReturn,
											Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: &intType}},
										}}},
									},
								}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: &ast.Expression{Kind: ast.ExprIdent, Name: "next"},
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
	if len(mainFn.Locals) != 1 || mainFn.Locals[0].ID != "local.next" || hirTypeString(&program.TypeTable, mainFn.Locals[0].Type) != "function() Int64" {
		t.Fatalf("expected next local to keep function literal signature, got %#v", mainFn.Locals)
	}
}

func TestLowerFunctionIdentifierLocalDeclType(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "AddOne",
					Params:  []ast.Field{{Name: "x", Type: intType}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:     ast.ExprBinary,
							Operator: "+",
							Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "x"},
							Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
						}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names:  []string{"f"},
								Values: []ast.Expression{{Kind: ast.ExprIdent, Name: "AddOne"}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: &ast.Expression{Kind: ast.ExprIdent, Name: "f"},
							Args:   []ast.Expression{{Kind: ast.ExprLiteral, Literal: "41", Type: &intType}},
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
	if len(mainFn.Locals) != 1 || mainFn.Locals[0].ID != "local.f" || hirTypeString(&program.TypeTable, mainFn.Locals[0].Type) != "function(Int64) Int64" {
		t.Fatalf("expected f local to keep function identifier signature, got %#v", mainFn.Locals)
	}
}
