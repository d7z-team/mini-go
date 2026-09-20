package lower

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerControlFlowUsesExplicitLabels(t *testing.T) {
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtIf,
						Cond: &ast.Expression{Kind: ast.ExprLiteral, Literal: "true", Type: boolType},
						Body: ast.BlockStmt{},
					}, {
						Kind: ast.StmtFor,
						Cond: &ast.Expression{Kind: ast.ExprLiteral, Literal: "false", Type: boolType},
						Body: ast.BlockStmt{},
					}, {
						Kind: ast.StmtSwitch,
						Expr: &ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: intType},
						Cases: []ast.CaseClause{{
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "1", Type: intType}},
							Body:   ast.BlockStmt{},
						}, {
							Default: true,
							Body:    ast.BlockStmt{},
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	body := program.Functions[0].Body
	if countStatements(body, ir.StmtJumpIf) != 3 {
		t.Fatalf("expected three jump_if statements, got %#v", body)
	}
	if countStatements(body, ir.StmtLabel) < 8 {
		t.Fatalf("expected synthetic labels, got %#v", body)
	}
}

func TestLowerSwitchTagEvaluatesOnce(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name:    "Next",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: intType}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtSwitch,
						Expr: &ast.Expression{
							Kind:   ast.ExprCall,
							Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Next"}),
						},
						Cases: []ast.CaseClause{{
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "1", Type: intType}},
						}, {
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: intType}},
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
	var tagLocal string
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtStoreLocal && strings.Contains(stmt.Local, "switch.tag") {
			if stmt.Expr.Kind != ir.ExprCallDirect || stmt.Expr.Function != "fn.Next" {
				t.Fatalf("expected switch tag direct call store, got %#v", stmt)
			}
			tagLocal = stmt.Local
			break
		}
	}
	if tagLocal == "" {
		t.Fatalf("expected switch tag temp store, got %#v", fn.Body)
	}
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtJumpIf && stmt.Expr.Kind == ir.ExprBinary {
			if stmt.Expr.Left == nil || stmt.Expr.Left.Kind != ir.ExprLocal || stmt.Expr.Left.Local != tagLocal {
				t.Fatalf("expected case condition to read switch tag temp %q, got %#v", tagLocal, stmt.Expr)
			}
		}
	}
}

func TestLowerSwitchFallthroughJumpsToNextCase(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtSwitch,
						Expr: &ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: intType},
						Cases: []ast.CaseClause{{
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "1", Type: intType}},
							Body: ast.BlockStmt{Stmts: []ast.Statement{{
								Kind: ast.StmtBranch,
								Op:   "fallthrough",
							}}},
						}, {
							Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: intType}},
							Body:   ast.BlockStmt{},
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
	var firstCaseLabel, secondCaseLabel string
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtJumpIf {
			if firstCaseLabel == "" {
				firstCaseLabel = stmt.Label
				continue
			}
			secondCaseLabel = stmt.Label
			break
		}
	}
	if firstCaseLabel == "" || secondCaseLabel == "" || firstCaseLabel == secondCaseLabel {
		t.Fatalf("expected two distinct case labels, got first=%q second=%q body=%#v", firstCaseLabel, secondCaseLabel, fn.Body)
	}
	for i, stmt := range fn.Body {
		if stmt.Kind == ir.StmtLabel && stmt.Label == firstCaseLabel {
			if i+1 >= len(fn.Body) || fn.Body[i+1].Kind != ir.StmtJump || fn.Body[i+1].Label != secondCaseLabel {
				t.Fatalf("expected fallthrough jump from first case to second case, got %#v", fn.Body)
			}
			return
		}
	}
	t.Fatalf("expected first case label %q in body %#v", firstCaseLabel, fn.Body)
}

func TestLowerSelectDefaultAndEmptySelect(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name:    "WithDefault",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtSelect,
						Cases: []ast.CaseClause{{
							Default: true,
							Body: ast.BlockStmt{Stmts: []ast.Statement{{
								Kind:    ast.StmtReturn,
								Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: intType}},
							}}},
						}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name: "Empty",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtSelect,
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	withDefault, ok := findFunction(program.Functions, "fn.WithDefault")
	if !ok {
		t.Fatalf("expected WithDefault function, got %#v", program.Functions)
	}
	if countStatements(withDefault.Body, ir.StmtReturn) != 1 || countStatements(withDefault.Body, ir.StmtLabel) == 0 {
		t.Fatalf("expected default select body with end label, got %#v", withDefault.Body)
	}
	empty, ok := findFunction(program.Functions, "fn.Empty")
	if !ok {
		t.Fatalf("expected Empty function, got %#v", program.Functions)
	}
	if len(empty.Body) < 1 || empty.Body[0].Kind != ir.StmtSelect || len(empty.Body[0].SelectCases) != 0 || empty.Body[0].SelectDefault {
		t.Fatalf("expected empty blocking select, got %#v", empty.Body)
	}
}

func TestLowerSelectDescribesReceiveDestinations(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	chanType := ast.TypeExpr{Kind: ast.TypeChan, Elem: &intType}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name:    "Main",
					Params:  []ast.Field{{Name: "ch", Type: chanType}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: ast.ValueDecl{
							Names: []string{"out"},
							Type:  intType,
							Values: []ast.Expression{{
								Kind:    ast.ExprLiteral,
								Literal: "0",
								Type:    intType,
							}},
						}}},
					}, {
						Kind: ast.StmtSelect,
						Cases: []ast.CaseClause{{
							Comm: &ast.Statement{
								Kind: ast.StmtAssign,
								Op:   "=",
								Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "out"}},
								Right: []ast.Expression{{
									Kind:    ast.ExprReceive,
									Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "ch"}),
								}},
							},
						}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "out"}},
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
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtSelect {
			if stmt.SelectDefault || len(stmt.SelectCases) != 1 {
				t.Fatalf("invalid receive selection: %#v", stmt)
			}
			selected := stmt.SelectCases[0]
			if selected.Channel == "" || selected.Send != "" || selected.Value == "" || selected.OK == "" {
				t.Fatalf("invalid receive destinations: %#v", selected)
			}
			return
		}
	}
	t.Fatal("missing receive selection")
}

func TestLowerSelectBindsSendValueBeforeCommunication(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	chanType := ast.TypeExpr{Kind: ast.TypeChan, Elem: &intType}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name:    "Main",
					Params:  []ast.Field{{Name: "ch", Type: chanType}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtSelect,
						Cases: []ast.CaseClause{{
							Comm: &ast.Statement{
								Kind:  ast.StmtSend,
								Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "ch"}},
								Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: intType}},
							},
						}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: intType}},
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
	boundValue := ""
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtStoreLocal && strings.Contains(stmt.Local, "select.send") {
			boundValue = stmt.Local
		}
		if stmt.Kind == ir.StmtSelect {
			if boundValue == "" || stmt.SelectDefault || len(stmt.SelectCases) != 1 || stmt.SelectCases[0].Send != boundValue {
				t.Fatalf("send does not use its evaluated operand: %#v", stmt)
			}
			return
		}
	}
	t.Fatal("missing send selection")
}

func TestLowerSelectDefaultKeepsCommunicationCases(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	chanType := ast.TypeExpr{Kind: ast.TypeChan, Elem: &intType}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: ast.FuncDecl{
					Name:    "Main",
					Params:  []ast.Field{{Name: "ch", Type: chanType}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: ast.ValueDecl{
							Names: []string{"out"},
							Type:  intType,
							Values: []ast.Expression{{
								Kind:    ast.ExprLiteral,
								Literal: "0",
								Type:    intType,
							}},
						}}},
					}, {
						Kind: ast.StmtSelect,
						Cases: []ast.CaseClause{{
							Comm: &ast.Statement{
								Kind: ast.StmtAssign,
								Op:   "=",
								Left: []ast.Expression{{Kind: ast.ExprIdent, Name: "out"}},
								Right: []ast.Expression{{
									Kind:    ast.ExprReceive,
									Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "ch"}),
								}},
							},
						}, {
							Comm: &ast.Statement{
								Kind:  ast.StmtSend,
								Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "ch"}},
								Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: intType}},
							},
						}, {
							Default: true,
						}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "out"}},
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
	for _, stmt := range fn.Body {
		if stmt.Kind == ir.StmtSelect {
			if !stmt.SelectDefault || len(stmt.SelectCases) != 2 || stmt.SelectCases[0].Value == "" || stmt.SelectCases[1].Send == "" {
				t.Fatalf("invalid default selection: %#v", stmt)
			}
			return
		}
	}
	t.Fatal("missing default selection")
}
