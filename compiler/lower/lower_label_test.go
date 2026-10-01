package lower

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerLabelAndGotoStatements(t *testing.T) {
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
								Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
							},
						}},
					}, {
						Kind:  ast.StmtBranch,
						Op:    "goto",
						Label: "done",
					}, {
						Kind: ast.StmtAssign,
						Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
						Right: []ast.Expression{{
							Kind:    ast.ExprLiteral,
							Literal: "99",
							Type:    &intType,
						}},
					}, {
						Kind:  ast.StmtLabel,
						Label: "done",
						Body: ast.BlockStmt{Stmts: []ast.Statement{{
							Kind:    ast.StmtReturn,
							Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "x"}},
						}}},
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
	var sawGoto, sawLabel bool
	for _, stmt := range mainFn.Body {
		sawGoto = sawGoto || stmt.Kind == ir.StmtJump && stmt.Label == "user.done"
		sawLabel = sawLabel || stmt.Kind == ir.StmtLabel && stmt.Label == "user.done"
	}
	if !sawGoto || !sawLabel {
		t.Fatalf("expected goto and label for user.done, got %#v", mainFn.Body)
	}
}

func TestLowerLabeledBreakAndContinueStatements(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	identX := func() ast.Expression {
		return ast.Expression{Kind: ast.ExprIdent, Name: "x"}
	}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
	loop := func(body ast.BlockStmt) ast.Statement {
		return ast.Statement{
			Kind: ast.StmtFor,
			Cond: ptrExpr(ast.Expression{
				Kind:     ast.ExprBinary,
				Operator: "<",
				Type:     &boolType,
				Left:     ptrExpr(identX()),
				Right:    ptrExpr(intLiteral("5")),
			}),
			Post: &ast.Statement{
				Kind: ast.StmtAssign,
				Left: []ast.Expression{identX()},
				Right: []ast.Expression{{
					Kind:     ast.ExprBinary,
					Operator: "+",
					Left:     ptrExpr(identX()),
					Right:    ptrExpr(intLiteral("1")),
				}},
			},
			Body: body,
		}
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
								Names:  []string{"x"},
								Type:   intType,
								Values: []ast.Expression{intLiteral("0")},
							},
						}},
					}, {
						Kind:  ast.StmtLabel,
						Label: "Outer",
						Body: ast.BlockStmt{Stmts: []ast.Statement{loop(ast.BlockStmt{Stmts: []ast.Statement{
							loop(ast.BlockStmt{Stmts: []ast.Statement{{
								Kind:  ast.StmtBranch,
								Op:    "continue",
								Label: "Outer",
							}, {
								Kind:  ast.StmtBranch,
								Op:    "break",
								Label: "Outer",
							}}}),
						}})}},
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
	var sawOuterContinue, sawOuterBreak bool
	var outerPost, outerEnd string
	// The outer loop's post/end labels occur after the inner loop's labels.
	for _, stmt := range mainFn.Body {
		if stmt.Kind == ir.StmtLabel && strings.HasPrefix(stmt.Label, ".for.post.") {
			outerPost = stmt.Label
		}
		if stmt.Kind == ir.StmtLabel && strings.HasPrefix(stmt.Label, ".for.end.") {
			outerEnd = stmt.Label
		}
	}
	for _, stmt := range mainFn.Body {
		sawOuterContinue = sawOuterContinue || stmt.Kind == ir.StmtJump && stmt.Label == outerPost
		sawOuterBreak = sawOuterBreak || stmt.Kind == ir.StmtJump && stmt.Label == outerEnd
	}
	if !sawOuterContinue || !sawOuterBreak {
		t.Fatalf("expected labeled branch to target outer loop post/end labels, got %#v", mainFn.Body)
	}
}
