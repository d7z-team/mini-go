package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestLowerTypeAssertExpression(t *testing.T) {
	anyType := ast.TypeExpr{Kind: ast.TypeName, Name: "Any"}
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
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind: ast.ExprAssert,
							Type: &intType,
							Operand: &ast.Expression{
								Kind: ast.ExprConvert,
								Type: &anyType,
								Operand: &ast.Expression{
									Kind:    ast.ExprLiteral,
									Literal: "42",
									Type:    &intType,
								},
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
	assertion := fn.Body[0].Results[0]
	if assertion.Kind != ir.ExprTypeAssert || hirTypeString(&program.TypeTable, assertion.Type) != "Int64" {
		t.Fatalf("expected type_assert Int64, got %#v", assertion)
	}
}

func TestLowerInterfaceTypeSignature(t *testing.T) {
	byteType := ast.TypeExpr{Kind: ast.TypeName, Name: "Uint8"}
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	bufferType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &byteType}
	interfaceType := ast.TypeExpr{
		Kind: ast.TypeInterface,
		Methods: []ast.FuncDecl{{
			Name:    "Read",
			Params:  []ast.Field{{Name: "p", Type: bufferType}},
			Results: []ast.Field{{Type: intType}},
		}, {
			Name: "Close",
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
					Name:    "Use",
					Params:  []ast.Field{{Name: "r", Type: interfaceType}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "42", Type: &intType}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Use")
	if !ok {
		t.Fatalf("expected Use function, got %#v", program.Functions)
	}
	want := "function(interface{Read:function(Slice<Uint8>) Int64,Close:function()}) Int64"
	if got := types.FormatSignature(&program.TypeTable, fn.Signature); got != want {
		t.Fatalf("unexpected function signature:\nwant %s\n got %s", want, got)
	}
}

func TestLowerEmbeddedInterfaceTypeSignature(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	readerDecl := ast.Decl{
		Kind: ast.DeclType,
		Type: &ast.TypeDecl{
			Name: "Reader",
			Type: ast.TypeExpr{Kind: ast.TypeInterface, Methods: []ast.FuncDecl{{
				Name:    "Read",
				Results: []ast.Field{{Type: intType}},
			}}},
		},
	}
	interfaceType := ast.TypeExpr{
		Kind: ast.TypeInterface,
		Embeds: []ast.TypeExpr{{
			Kind: ast.TypeName,
			Name: "Reader",
		}, {
			Kind: ast.TypeName,
			Name: "io.Writer",
		}},
		Methods: []ast.FuncDecl{{
			Name:    "Close",
			Results: []ast.Field{{Type: intType}},
		}},
	}
	program, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{readerDecl, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:   "Use",
					Params: []ast.Field{{Name: "r", Type: interfaceType}},
					Body:   ast.BlockStmt{},
				},
			}},
		}},
	}, Options{Dependencies: testDependencies([]check.DependencyExport{{
		ModulePath: "io",
		Name:       "Writer",
		Kind:       check.ObjectType,
		Type:       "io.Writer",
		Underlying: "interface{Write:function(Slice<Uint8>) Int64}",
		Methods: []check.DependencyTypeMethod{{
			Name:       "Write",
			Signature:  "function(Slice<Uint8>) Int64",
			ModulePath: "io",
		}},
	}})})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Use")
	if !ok {
		t.Fatalf("expected Use function, got %#v", program.Functions)
	}
	want := "function(interface{example/main.Reader,io.Writer,Close:function() Int64})"
	if got := types.FormatSignature(&program.TypeTable, fn.Signature); got != want {
		t.Fatalf("unexpected function signature:\nwant %s\n got %s", want, got)
	}
}
