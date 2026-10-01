package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/emit"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLowerDirectFunctionCallToArtifact(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "AddOne",
					Params: []ast.Field{{
						Name: "x",
						Type: intType,
					}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:     ast.ExprBinary,
							Operator: "+",
							Left: &ast.Expression{
								Kind: ast.ExprIdent,
								Name: "x",
							},
							Right: &ast.Expression{
								Kind:    ast.ExprLiteral,
								Literal: "1",
								Type:    &intType,
							},
						}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind: ast.ExprCall,
							Callee: &ast.Expression{
								Kind: ast.ExprIdent,
								Name: "AddOne",
							},
							Args: []ast.Expression{{
								Kind:    ast.ExprLiteral,
								Literal: "41",
								Type:    &intType,
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

	artifact, err := emit.Lower(program)
	if err != nil {
		t.Fatalf("HIR emit failed: %v", err)
	}
	var mainInstructions []ir.Instruction
	for _, function := range artifact.Functions {
		if function.ID == "fn.Main" {
			mainInstructions, err = function.Operations()
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	call, ok := findInstruction(mainInstructions, ir.OpCallDirect)
	if !ok {
		t.Fatalf("Main does not contain a direct call: %#v", mainInstructions)
	}
	var payload ir.CallPayload
	if err := ir.ReadInstructionPayload(call.Payload, &payload); err != nil {
		t.Fatalf("decode direct call payload: %v", err)
	}
	if payload.Function != "fn.AddOne" || payload.ArgCount != 1 || payload.ResultCount != 1 {
		t.Fatalf("unexpected direct call payload: %#v", payload)
	}
}

func TestLowerDirectCallResultCounts(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{Name: "Log"},
			}, {
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
						Kind: ast.StmtExpr,
						Expr: &ast.Expression{
							Kind:   ast.ExprCall,
							Callee: &ast.Expression{Kind: ast.ExprIdent, Name: "Log"},
						},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: &ast.Expression{Kind: ast.ExprIdent, Name: "Pair"},
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
	stmtCall := fn.Body[0].Expr
	if stmtCall.Kind != hir.ExprCallDirect || stmtCall.ResultCount != 0 {
		t.Fatalf("expected void direct call result count 0, got %#v", stmtCall)
	}
	returnCall := fn.Body[1].Results[0]
	if returnCall.Kind != hir.ExprCallDirect || returnCall.ResultCount != 2 {
		t.Fatalf("expected multi-result direct call count 2, got %#v", returnCall)
	}
}

func TestLowerVariadicDirectCallPacksArgs(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Sum",
					Params:  []ast.Field{{Name: "xs", Type: intType, Variadic: true}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:   ast.ExprCall,
							Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Sum"}),
							Args: []ast.Expression{
								{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
								{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
								{Kind: ast.ExprLiteral, Literal: "3", Type: &intType},
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
	sum, ok := findFunction(program.Functions, "fn.Sum")
	if !ok {
		t.Fatalf("expected Sum function, got %#v", program.Functions)
	}
	if types.FormatSignature(&program.TypeTable, sum.Signature) != "function(variadic Slice<Int64>) Int64" || !sum.Signature.Variadic || len(sum.Locals) != 1 || hirTypeString(&program.TypeTable, sum.Locals[0].Type) != "Slice<Int64>" {
		t.Fatalf("expected variadic parameter to lower as Slice<Int64>, got %#v", sum)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	call := mainFn.Body[0].Results[0]
	if call.Kind != hir.ExprCallDirect || len(call.Args) != 1 || call.Args[0].Kind != hir.ExprSequence {
		t.Fatalf("expected direct call with one packed array arg, got %#v", call)
	}
	if hirTypeString(&program.TypeTable, call.Args[0].Type) != "Slice<Int64>" || len(call.Args[0].Elements) != 3 {
		t.Fatalf("unexpected packed variadic array: %#v", call.Args[0])
	}
}
