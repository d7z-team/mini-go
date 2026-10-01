package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/emit"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLowerTopLevelConstAndVar(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Answer"},
					Type:   intType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}},
				},
			}, {
				Kind: ast.DeclVar,
				Var: &ast.ValueDecl{
					Names:  []string{"Offset"},
					Type:   intType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:     ast.ExprBinary,
							Operator: "+",
							Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "Answer"},
							Right:    &ast.Expression{Kind: ast.ExprIdent, Name: "Offset"},
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.Constants) != 1 || program.Constants[0].ID != "const.Answer" {
		t.Fatalf("expected top-level constant, got %#v", program.Constants)
	}
	if len(program.Exports) != 3 {
		t.Fatalf("expected const, global, and function exports, got %#v", program.Exports)
	}
	if program.Exports[0].Name != "Answer" || program.Exports[0].Kind != "const" || program.Exports[0].ID != "const.Answer" || hirTypeString(&program.TypeTable, program.Exports[0].Type) != "Int64" {
		t.Fatalf("expected exported top-level constant, got %#v", program.Exports)
	}
	if len(program.Globals) != 1 || program.Globals[0].ID != "global.Offset" {
		t.Fatalf("expected top-level global, got %#v", program.Globals)
	}
	initFn, ok := findFunction(program.Functions, "fn.init")
	if !ok || len(initFn.Body) != 1 || initFn.Body[0].Kind != hir.StmtStoreGlobal {
		t.Fatalf("expected synthetic init storing global, got %#v", program.Functions)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	returnExpr := mainFn.Body[0].Results[0]
	if returnExpr.Kind != hir.ExprBinary || returnExpr.Left == nil || returnExpr.Right == nil || returnExpr.Right.Kind != hir.ExprGlobal {
		t.Fatalf("expected folded const plus global return expression, got %#v", returnExpr)
	}
	if returnExpr.Left.Kind != hir.ExprLiteral || hirTypeString(&program.TypeTable, returnExpr.Left.Type) != "Int64" || string(returnExpr.Left.Value) != "40" {
		t.Fatalf("expected const side to fold to Int64 literal, got %#v", returnExpr.Left)
	}
}

func TestLowerMultipleInitFunctionsThroughSyntheticEntry(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	_, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/init",
		Package:    "main",
		Files: []ast.File{{
			Path: "invalid.mgo",
			Decls: []ast.Decl{{Kind: ast.DeclFunc, Func: &ast.FuncDecl{
				Name:   "init",
				Params: []ast.Field{{Name: "value", Type: intType}},
			}}},
		}},
	})
	if len(diagnostics) == 0 || diagnostics[0].Code != "hirgen.init.signature" {
		t.Fatalf("expected init signature diagnostic, got %#v", diagnostics)
	}

	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/init",
		Package:    "main",
		Files: []ast.File{{
			Path:  "first.mgo",
			Decls: []ast.Decl{{Kind: ast.DeclFunc, Func: &ast.FuncDecl{Name: "init"}}},
		}, {
			Path:  "second.mgo",
			Decls: []ast.Decl{{Kind: ast.DeclFunc, Func: &ast.FuncDecl{Name: "init"}}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	first, ok := findFunction(program.Functions, "fn.user_init.0")
	if !ok || first.Name != "init" {
		t.Fatalf("expected first user init function, got %#v", program.Functions)
	}
	second, ok := findFunction(program.Functions, "fn.user_init.1")
	if !ok || second.Name != "init" {
		t.Fatalf("expected second user init function, got %#v", program.Functions)
	}
	entry, ok := findFunction(program.Functions, "fn.init")
	if !ok || len(entry.Body) != 2 {
		t.Fatalf("expected synthetic init entry with two calls, got %#v", program.Functions)
	}
	for i, want := range []string{"fn.user_init.0", "fn.user_init.1"} {
		if entry.Body[i].Kind != hir.StmtExpr || entry.Body[i].Expr.Kind != hir.ExprCallDirect || entry.Body[i].Expr.Function != want {
			t.Fatalf("expected init call %d to %s, got %#v", i, want, entry.Body[i])
		}
	}
}

func TestLowerTopLevelNamedConstAlias(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Base"},
					Type:   intType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Answer"},
					Values: []ast.Expression{{Kind: ast.ExprIdent, Name: "Base"}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
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
	if len(program.Constants) != 2 || program.Constants[1].ID != "const.Answer" || hirTypeString(&program.TypeTable, program.Constants[1].Type) != "Int64" || string(program.Constants[1].Value) != "40" {
		t.Fatalf("expected named constant alias to copy value, got %#v", program.Constants)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	returnExpr := mainFn.Body[0].Results[0]
	if returnExpr.Kind != hir.ExprLiteral || hirTypeString(&program.TypeTable, returnExpr.Type) != "Int64" || string(returnExpr.Value) != "40" {
		t.Fatalf("expected alias constant to fold to Int64 literal, got %#v", returnExpr)
	}
}

func TestLowerTypeAliasDoesNotCreateRuntimeType(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	aliasType := ast.TypeExpr{Kind: ast.TypeName, Name: "MyInt"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "MyInt", Type: intType, Alias: true},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Params:  []ast.Field{{Name: "v", Type: aliasType}},
					Results: []ast.Field{{Type: aliasType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "v"}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if declarations := program.TypeTable.DefinedNamed(program.ModulePath); len(declarations) != 0 {
		t.Fatalf("alias declarations must not create runtime type identities, got %#v", declarations)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if got := types.FormatSignature(&program.TypeTable, mainFn.Signature); got != "function(Int64) Int64" {
		t.Fatalf("expected alias to be resolved in signature, got %q", got)
	}
	if len(mainFn.Locals) != 1 || hirTypeString(&program.TypeTable, mainFn.Locals[0].Type) != "Int64" {
		t.Fatalf("expected alias local type to resolve to Int64, got %#v", mainFn.Locals)
	}
}

func TestLowerNamedResultBareReturn(t *testing.T) {
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
					Results: []ast.Field{{Name: "answer", Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:  ast.StmtAssign,
						Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "answer"}},
						Right: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: &intType}},
					}, {
						Kind: ast.StmtReturn,
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
	if len(mainFn.Locals) != 1 || mainFn.Locals[0].ID != "local.answer" || hirTypeString(&program.TypeTable, mainFn.Locals[0].Type) != "Int64" {
		t.Fatalf("expected named result local, got %#v", mainFn.Locals)
	}
	if len(mainFn.Body) < 2 || mainFn.Body[1].Kind != hir.StmtReturn || len(mainFn.Body[1].Results) != 1 ||
		mainFn.Body[1].Results[0].Kind != hir.ExprLocal || mainFn.Body[1].Results[0].Local != "local.answer" {
		t.Fatalf("expected bare return to load named result local, got %#v", mainFn.Body)
	}
}

func TestLowerMultiResultLocalVarInfersFunctionResultTypes(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Pair",
					Results: []ast.Field{{Type: intType}, {Type: stringType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{
							{Kind: ast.ExprLiteral, Literal: "42", Type: &intType},
							{Kind: ast.ExprLiteral, Literal: `"ok"`, Type: &stringType},
						},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"n", "s"},
								Values: []ast.Expression{{
									Kind:   ast.ExprCall,
									Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Pair"}),
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
	localTypes := map[string]string{}
	for _, local := range mainFn.Locals {
		localTypes[local.Name] = hirTypeString(&program.TypeTable, local.Type)
	}
	if localTypes["n"] != "Int64" || localTypes["s"] != "String" {
		t.Fatalf("expected multi-result local types, got %#v", mainFn.Locals)
	}
}

func TestLowerBlankIdentifierDiscardTarget(t *testing.T) {
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
						Kind: ast.StmtReturn,
						Results: []ast.Expression{
							{Kind: ast.ExprLiteral, Literal: "40", Type: &intType},
							{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
						},
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
								Names: []string{"_", "y"},
								Values: []ast.Expression{{
									Kind:   ast.ExprCall,
									Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Pair"}),
								}},
							},
						}},
					}, {
						Kind:  ast.StmtAssign,
						Left:  []ast.Expression{{Kind: ast.ExprIdent, Name: "_"}},
						Right: []ast.Expression{{Kind: ast.ExprIdent, Name: "y"}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "y"}},
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
	for _, local := range mainFn.Locals {
		if local.Name == "_" {
			t.Fatalf("blank identifier must not create a local slot, got %#v", mainFn.Locals)
		}
	}
	if len(mainFn.Body) < 2 || mainFn.Body[0].Kind != hir.StmtStoreResults || len(mainFn.Body[0].Targets) != 2 ||
		mainFn.Body[0].Targets[0].Kind != "discard" || mainFn.Body[0].Targets[1].Kind != "local" {
		t.Fatalf("expected discard/local store_results targets, got %#v", mainFn.Body)
	}
	if mainFn.Body[1].Kind != hir.StmtExpr {
		t.Fatalf("expected blank assignment to lower to expression discard, got %#v", mainFn.Body)
	}
	artifact, err := emit.Lower(program)
	if err != nil {
		t.Fatalf("HIR emit failed: %v", err)
	}
	fn, ok := findIRFunction(artifact.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected IR Main function, got %#v", artifact.Functions)
	}
	for _, operation := range fn.Code.Instructions {
		if operation.Op != ir.OpCallDirect {
			continue
		}
		operands := fn.Code.Operands[operation.Operands]
		if len(operands.Outputs) != 2 {
			t.Fatalf("pair result destinations = %#v", operands.Outputs)
		}
		for _, released := range operands.Release {
			if released == operands.Outputs[0] {
				return
			}
		}
		t.Fatal("discarded result remains live after its call")
	}
	t.Fatal("pair call not emitted")
}
