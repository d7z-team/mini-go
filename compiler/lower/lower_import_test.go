package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/parser"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
)

func TestLowerRequiresImportedMemberSelection(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", "package main\nimport \"example/lib\"\nvar value = lib.Value\n")
	options := Options{Dependencies: []check.DependencyPackage{{ModulePath: "example/lib", Members: []check.DependencyExport{{Name: "Value", Kind: check.ObjectVar, Type: "Int"}}}}}
	checked := check.WithOptions(parsed.Program, check.AnalyzeOptions{Dependencies: options.Dependencies})
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatal(checked.Info.Diagnostics)
	}
	for node, selection := range checked.Info.Selections {
		if selection.Kind == check.SelectionPackageMember {
			delete(checked.Info.Selections, node)
		}
	}
	_, diagnostics := Lower(checked, options)
	requireDiagnostic(t, diagnostics, "hirgen.semantic.selector")
}

func TestLowerImportSelectorBecomesExportRequirement(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "example/lib",
					Alias: "lib",
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
							Callee: ptrExpr(ast.Expression{
								Kind: ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{
									Kind: ast.ExprIdent,
									Name: "lib",
								}),
								Field: "Add",
							}),
							Args: []ast.Expression{{
								Kind:    ast.ExprLiteral,
								Literal: "20",
								Type:    &intType,
							}, {
								Kind:    ast.ExprLiteral,
								Literal: "22",
								Type:    &intType,
							}},
						}},
					}}},
				},
			}},
		}},
	}, Options{Dependencies: testDependencies([]check.DependencyExport{{ModulePath: "example/lib", Name: "Add", Kind: check.ObjectFunc, Type: "function(Int64, Int64) Int64"}})})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.Requirements) != 1 || program.Requirements[0].ModulePath != "example/lib" {
		t.Fatalf("expected source requirement, got %#v", program.Requirements)
	}
	if len(program.Requirements[0].Exports) != 1 || program.Requirements[0].Exports[0] != "Add" {
		t.Fatalf("expected Add export requirement, got %#v", program.Requirements[0].Exports)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	call := fn.Body[0].Results[0]
	if call.Kind != ir.ExprCallValue || call.Operand == nil || call.Operand.Kind != ir.ExprLoadExport {
		t.Fatalf("expected module member call, got %#v", call)
	}
}

func TestLowerImportOnlyPackageInitializesDependencies(t *testing.T) {
	program, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind:   ast.DeclImport,
				Import: &ast.ImportDecl{Path: "example/lib", Alias: "_"},
			}},
		}},
	}, Options{Dependencies: []check.DependencyPackage{{ModulePath: "example/lib"}}})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	init, ok := findFunction(program.Functions, "fn.init")
	if !ok || len(init.Body) != 1 || init.Body[0].Kind != ir.StmtInitModule || init.Body[0].Module != "example/lib" {
		t.Fatalf("dependency initialization = %#v", init.Body)
	}
}

func TestLowerAllowsDuplicateImportAliasForSamePathAcrossFiles(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	_, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "a.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "fmt",
					Alias: "fmt",
				},
			}, selectorReturnFuncDecl("A", "fmt", "Value", intType, "a.mgo")},
		}, {
			Path: "b.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "fmt",
					Alias: "fmt",
				},
			}, selectorReturnFuncDecl("B", "fmt", "Value", intType, "b.mgo")},
		}},
	}, Options{Dependencies: testDependencies([]check.DependencyExport{{
		ModulePath: "fmt",
		Name:       "Value",
		Kind:       check.ObjectFunc,
		Type:       "function() Int64",
	}})})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
}

func TestLowerAllowsDuplicateImportAliasForDifferentPathsAcrossFiles(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	_, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "a.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "fmt",
					Alias: "lib",
				},
			}, selectorReturnFuncDecl("Left", "lib", "Value", intType, "a.mgo")},
		}, {
			Path: "b.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "strings",
					Alias: "lib",
				},
			}, selectorReturnFuncDecl("Right", "lib", "Value", intType, "b.mgo")},
		}},
	}, Options{Dependencies: testDependencies([]check.DependencyExport{{
		ModulePath: "fmt",
		Name:       "Value",
		Kind:       check.ObjectFunc,
		Type:       "function() Int64",
	}, {
		ModulePath: "strings",
		Name:       "Value",
		Kind:       check.ObjectFunc,
		Type:       "function() Int64",
	}})})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
}

func selectorReturnFuncDecl(name, alias, selector string, resultType ast.TypeExpr, file string) ast.Decl {
	aliasSpan := source.Span{Start: source.Position{File: file}, End: source.Position{File: file}}
	return ast.Decl{
		Kind: ast.DeclFunc,
		Func: &ast.FuncDecl{
			Name:    name,
			Results: []ast.Field{{Type: resultType}},
			Body: ast.BlockStmt{Stmts: []ast.Statement{{
				Kind: ast.StmtReturn,
				Results: []ast.Expression{{
					Kind: ast.ExprCall,
					Callee: ptrExpr(ast.Expression{
						Kind: ast.ExprSelector,
						Operand: ptrExpr(ast.Expression{
							Kind: ast.ExprIdent,
							Name: alias,
							Span: aliasSpan,
						}),
						Field: selector,
					}),
				}},
			}}},
		},
	}
}

func TestLowerRejectsDuplicateImportAliasInSameFile(t *testing.T) {
	_, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "fmt",
					Alias: "lib",
				},
			}, {
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "strings",
					Alias: "lib",
				},
			}},
		}},
	}, Options{Dependencies: []check.DependencyPackage{{ModulePath: "fmt"}, {ModulePath: "strings"}}})
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "semantic.import.alias.duplicate" {
			return
		}
	}
	t.Fatalf("expected duplicate alias diagnostic, got %#v", diagnostics)
}

func TestLowerImportedSelectorTypeConversion(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgramWithOptions(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclImport,
				Import: &ast.ImportDecl{
					Path:  "example/lib",
					Alias: "lib",
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: ast.TypeExpr{Kind: ast.TypeName, Name: "lib.Score"}}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind: ast.ExprCall,
							Callee: ptrExpr(ast.Expression{
								Kind: ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{
									Kind: ast.ExprIdent,
									Name: "lib",
								}),
								Field: "Score",
							}),
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
	}, Options{Dependencies: testDependencies([]check.DependencyExport{{
		ModulePath: "example/lib",
		Name:       "Score",
		Kind:       check.ObjectType,
		Type:       "example/lib.Score",
		Underlying: "Int64",
	}})})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.Requirements) != 1 || program.Requirements[0].ModulePath != "example/lib" {
		t.Fatalf("expected source requirement, got %#v", program.Requirements)
	}
	if len(program.Requirements[0].Exports) != 1 || program.Requirements[0].Exports[0] != "Score" {
		t.Fatalf("expected Score export requirement, got %#v", program.Requirements[0].Exports)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	converted := fn.Body[0].Results[0]
	if converted.Kind != ir.ExprConvert || hirTypeString(&program.TypeTable, converted.Type) != "example/lib.Score" || converted.Operand == nil || converted.Operand.Kind != ir.ExprLiteral {
		t.Fatalf("expected imported selector type conversion, got %#v", converted)
	}
}

func TestLowerReportsInvalidDependencyMetadata(t *testing.T) {
	program := ast.Program{ModulePath: "example/main", Package: "main"}
	checked := check.Check(program)
	_, diagnostics := Lower(checked, Options{Dependencies: testDependencies([]check.DependencyExport{{
		ModulePath: "example/lib",
		Name:       "Value",
	}})})
	requireDiagnostic(t, diagnostics, "hirgen.dependency.kind")
}
