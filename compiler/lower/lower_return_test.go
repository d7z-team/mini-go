package lower

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerBareReturnRequiresNamedResults(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	_, diagnostics := lowerTestProgram(ast.Program{
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
						Kind: ast.StmtReturn,
					}}},
				},
			}},
		}},
	})
	requireDiagnostic(t, diagnostics, "hirgen.return.bare")
}

func TestLowerLocalConstDecl(t *testing.T) {
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
							Kind: ast.DeclConst,
							Const: &ast.ValueDecl{
								Names:  []string{"Base"},
								Type:   intType,
								Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclConst,
							Const: &ast.ValueDecl{
								Names: []string{"Answer"},
								Type:  intType,
								Values: []ast.Expression{{
									Kind:     ast.ExprBinary,
									Operator: "+",
									Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "Base"},
									Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
								}},
							},
						}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "Answer"}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.Constants) != 2 || program.Constants[1].Name != "Answer" || hirTypeString(&program.TypeTable, program.Constants[1].Type) != "Int64" || string(program.Constants[1].Value) != "42" {
		t.Fatalf("expected folded local constants, got %#v", program.Constants)
	}
	if strings.HasPrefix(program.Constants[1].ID, "const.Answer") {
		t.Fatalf("expected local constant to use scoped id, got %#v", program.Constants[1])
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	returnExpr := mainFn.Body[0].Results[0]
	if returnExpr.Kind != ir.ExprLiteral || hirTypeString(&program.TypeTable, returnExpr.Type) != "Int64" || string(returnExpr.Value) != "42" {
		t.Fatalf("expected local constant to fold to Int64 literal, got %#v", returnExpr)
	}
}
