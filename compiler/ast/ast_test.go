package ast

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
)

func TestValidateProgramAcceptsTaggedUnionAST(t *testing.T) {
	file := source.File{Path: "main.mgo", Text: "package main\n"}
	span, ok := file.Span(0, len(file.Text))
	if !ok {
		t.Fatal("failed to build span")
	}
	intType := TypeExpr{Kind: TypeName, Name: "Int64"}
	program := Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []File{{
			ID:   "file.main",
			Path: "main.mgo",
			Span: span,
			Decls: []Decl{{
				Kind: DeclFunc,
				Span: span,
				Func: &FuncDecl{
					Name: "main",
					Results: []Field{{
						Type: intType,
					}},
					Body: BlockStmt{
						Stmts: []Statement{{
							Kind: StmtReturn,
							Results: []Expression{{
								Kind:    ExprLiteral,
								Literal: "42",
								Type:    &intType,
							}},
						}},
					},
				},
			}},
		}},
	}

	if diagnostics := ValidateProgram(program); len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
}

func TestValidateProgramRejectsDuplicateFunction(t *testing.T) {
	program := Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []File{{
			Path: "main.mgo",
			Decls: []Decl{{
				Kind: DeclFunc,
				Func: &FuncDecl{Name: "main"},
			}, {
				Kind: DeclFunc,
				Func: &FuncDecl{Name: "main"},
			}},
		}},
	}

	diagnostics := ValidateProgram(program)
	requireDiagnostic(t, diagnostics, "ast.func.name.duplicate")
}

func TestValidateProgramAllowsRepeatedBlankDeclarations(t *testing.T) {
	intType := TypeExpr{Kind: TypeName, Name: "Int64"}
	program := Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []File{{
			Path: "main.mgo",
			Decls: []Decl{{
				Kind: DeclType,
				Type: &TypeDecl{Name: "_", Type: intType},
			}, {
				Kind: DeclType,
				Type: &TypeDecl{Name: "_", Type: intType},
			}, {
				Kind: DeclFunc,
				Func: &FuncDecl{Name: "_"},
			}, {
				Kind: DeclFunc,
				Func: &FuncDecl{Name: "_"},
			}},
		}},
	}

	if diagnostics := ValidateProgram(program); len(diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", diagnostics)
	}
}

func TestValidateProgramRejectsBlankInterfaceMethod(t *testing.T) {
	program := Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []File{{
			Path: "main.mgo",
			Decls: []Decl{{
				Kind: DeclType,
				Type: &TypeDecl{
					Name: "I",
					Type: TypeExpr{Kind: TypeInterface, Methods: []FuncDecl{{Name: "_"}}},
				},
			}},
		}},
	}

	diagnostics := ValidateProgram(program)
	requireDiagnostic(t, diagnostics, "ast.type.interface.method.blank")
}

func TestValidateProgramRejectsBlankPackageName(t *testing.T) {
	program := Program{
		ModulePath: "example/main",
		Package:    "_",
		Files: []File{{
			Path: "main.mgo",
		}},
	}

	diagnostics := ValidateProgram(program)
	requireDiagnostic(t, diagnostics, "ast.package.blank")
}

func TestValidateProgramRejectsMalformedExpression(t *testing.T) {
	program := Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []File{{
			Path: "main.mgo",
			Decls: []Decl{{
				Kind: DeclFunc,
				Func: &FuncDecl{
					Name: "main",
					Body: BlockStmt{Stmts: []Statement{{
						Kind: StmtExpr,
						Expr: &Expression{Kind: ExprBinary},
					}}},
				},
			}},
		}},
	}

	diagnostics := ValidateProgram(program)
	requireDiagnostic(t, diagnostics, "ast.expr.binary.missing")
}

func requireDiagnostic(t *testing.T, diagnostics []source.Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if string(diagnostic.Code) == code {
			return
		}
	}
	t.Fatalf("expected diagnostic %q, got %#v", code, diagnostics)
}
