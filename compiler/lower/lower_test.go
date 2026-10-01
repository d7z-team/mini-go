package lower

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/emit"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLowerASTFunctionToHIR(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Answer",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:    ast.ExprLiteral,
							Literal: "42",
							Type:    &intType,
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if program.ModulePath != "example/main" {
		t.Fatalf("unexpected HIR program: %#v", program)
	}
	fn, ok := findFunction(program.Functions, "fn.Answer")
	if !ok {
		t.Fatalf("missing Answer function: %#v", program.Functions)
	}
	if fn.ID != "fn.Answer" || types.FormatSignature(&program.TypeTable, fn.Signature) != "function() Int64" {
		t.Fatalf("unexpected HIR function: %#v", fn)
	}
	if len(program.Exports) != 1 || program.Exports[0].Name != "Answer" {
		t.Fatalf("expected exported function, got %#v", program.Exports)
	}
}

func TestLowerASTVariadicFunctionMetadata(t *testing.T) {
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
					Params:  []ast.Field{{Name: "base", Type: intType}, {Name: "values", Type: intType, Variadic: true}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	function, ok := findFunction(program.Functions, "fn.Sum")
	if !ok || !function.Signature.Variadic {
		t.Fatalf("expected variadic HIR function metadata, got %#v", program.Functions)
	}
	artifact, err := emit.Lower(program)
	if err != nil {
		t.Fatalf("HIR emit failed: %v", err)
	}
	irFunction, ok := findIRFunction(artifact.Functions, "fn.Sum")
	if !ok || !irFunction.Signature.Variadic {
		t.Fatalf("expected variadic IR function metadata, got %#v", artifact.Functions)
	}
}

func TestLowerASTStatementSpanToPackageSymbols(t *testing.T) {
	file := source.File{ID: "file.main", Path: "main.mgo", Text: "package main\nfunc Main() int64 {\n\treturn 42\n}\n"}
	fileSpan, ok := file.Span(0, len(file.Text))
	if !ok {
		t.Fatal("failed to create file span")
	}
	returnOffset := strings.Index(file.Text, "return")
	returnSpan, ok := file.Span(returnOffset, returnOffset+len("return 42"))
	if !ok {
		t.Fatal("failed to create return span")
	}
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			ID:   file.ID,
			Path: file.Path,
			Hash: source.HashText(file.Text),
			Span: fileSpan,
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Span: returnSpan,
						Results: []ast.Expression{{
							Kind:    ast.ExprLiteral,
							Literal: "42",
							Type:    &intType,
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.DebugFiles) != 1 || program.DebugFiles[0].ID != "file.main" || program.DebugFiles[0].Path != "main.mgo" {
		t.Fatalf("unexpected HIR debug files: %#v", program.DebugFiles)
	}
	if program.DebugFiles[0].Hash != source.HashText(file.Text) || program.DebugSourceHash == "" {
		t.Fatalf("unexpected HIR debug hashes: source=%q files=%#v", program.DebugSourceHash, program.DebugFiles)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	if len(fn.Body) == 0 || len(fn.Body[0].SourcePoints) != 1 {
		t.Fatalf("expected first HIR statement location, got %#v", fn.Body)
	}
	if loc := fn.Body[0].SourcePoints[0]; loc.File != "main.mgo" || loc.Line != 3 || loc.Column != 1 {
		t.Fatalf("unexpected HIR location: %#v", loc)
	}
	artifact, symbols, err := emit.LowerUnvalidatedWithSymbols(program)
	if err != nil {
		t.Fatalf("HIR emit failed: %v", err)
	}
	if len(symbols.Files) != 1 {
		t.Fatalf("expected package symbol file metadata, got %#v", symbols)
	}
	if symbols.SourceHash != program.DebugSourceHash || symbols.Files[0].Hash != source.HashText(file.Text) {
		t.Fatalf("unexpected package symbol hashes: %#v", symbols)
	}
	if len(artifact.Functions) != 1 || len(symbols.Functions) != 1 {
		t.Fatalf("code and symbols function mismatch: code=%#v symbols=%#v", artifact.Functions, symbols.Functions)
	}
	if locations := symbols.Functions[0].Locations[0].Points; len(locations) != 1 || locations[0].File != "main.mgo" || locations[0].Line != 3 || locations[0].Column != 1 {
		t.Fatalf("unexpected instruction source points: %#v", locations)
	}
}

func TestLowerStructFieldTagsToIRTypeMetadata(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{
					Name: "User",
					Type: ast.TypeExpr{
						Kind: ast.TypeStruct,
						Fields: []ast.Field{{
							Name: "ID",
							Type: intType,
							Tag:  `json:"id" db:"user_id"`,
						}, {
							Name: "Name",
							Type: stringType,
							Tag:  `json:"name,omitempty"`,
						}},
					},
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
	declarations := artifact.TypeTable.DefinedNamed(artifact.Module.Path)
	if len(declarations) != 1 {
		t.Fatalf("expected IR type field metadata, got %#v", declarations)
	}
	shape, _ := artifact.TypeTable.Node(artifact.TypeTable.Underlying(types.Ref(declarations[0])))
	fields := shape.Fields
	if len(fields) != 2 {
		t.Fatalf("expected IR type field metadata, got %#v", declarations)
	}
	if fields[0].Name != "ID" || types.FormatWithTable(&artifact.TypeTable, fields[0].Type) != "Int64" || fields[0].Tag != `json:"id" db:"user_id"` {
		t.Fatalf("unexpected IR ID field metadata: %#v", fields[0])
	}
}

func TestLowerInterfaceTypeSetToIRTypeMetadata(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{
					Name: "Constraint",
					Type: ast.TypeExpr{
						Kind: ast.TypeInterface,
						Terms: []ast.TypeTerm{{
							Type:   intType,
							Approx: true,
						}, {
							Type: stringType,
						}},
						Methods: []ast.FuncDecl{{
							Name:    "Read",
							Results: []ast.Field{{Type: intType}},
						}},
					},
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
	declarations := artifact.TypeTable.DefinedNamed(artifact.Module.Path)
	formatted := ""
	if len(declarations) == 1 {
		formatted = types.FormatWithTable(&artifact.TypeTable, artifact.TypeTable.Underlying(types.Ref(declarations[0])))
	}
	if formatted != "interface{~Int64|String,Read:function() Int64}" {
		t.Fatalf("expected IR type-set metadata, got %q from %#v", formatted, declarations)
	}
}

func TestLowerVariadicFunctionTypeMetadata(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{
					Name: "Reducer",
					Type: ast.TypeExpr{
						Kind: ast.TypeFunc,
						Params: []ast.Field{{
							Type: intType,
						}, {
							Type:     intType,
							Variadic: true,
						}},
						Results: []ast.Field{{Type: intType}},
					},
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
	declarations := artifact.TypeTable.DefinedNamed(artifact.Module.Path)
	if len(declarations) != 1 {
		t.Fatalf("expected one variadic function type, got %#v", declarations)
	}
	signature, functionType := artifact.TypeTable.IsFunction(types.Ref(declarations[0]))
	if types.FormatWithTable(&artifact.TypeTable, artifact.TypeTable.Underlying(types.Ref(declarations[0]))) != "function(Int64, variadic Slice<Int64>) Int64" || !functionType || !signature.Variadic {
		t.Fatalf("expected IR variadic function type metadata, got %#v", declarations)
	}
}

func TestLowerVariadicMethodMetadata(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{
					Name: "Counter",
					Type: ast.TypeExpr{Kind: ast.TypeStruct},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:     "Sum",
					Receiver: &ast.Field{Name: "c", Type: counterType},
					Params: []ast.Field{{
						Name: "base",
						Type: intType,
					}, {
						Name:     "values",
						Type:     intType,
						Variadic: true,
					}},
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}, {
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{
					Name: "Summarizer",
					Type: ast.TypeExpr{
						Kind: ast.TypeInterface,
						Methods: []ast.FuncDecl{{
							Name: "Sum",
							Params: []ast.Field{{
								Type: intType,
							}, {
								Type:     intType,
								Variadic: true,
							}},
							Results: []ast.Field{{Type: intType}},
						}},
					},
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
	counter := artifactTypeByName(t, artifact, "Counter")
	if len(counter.Methods) != 1 || counter.Methods[0].Name != "Sum" || !counter.Methods[0].Signature.Variadic || types.FormatSignature(&artifact.TypeTable, counter.Methods[0].Signature) != "function(Int64, variadic Slice<Int64>) Int64" {
		t.Fatalf("expected Counter variadic method metadata, got %#v", counter.Methods)
	}
	summarizer := artifactTypeByName(t, artifact, "Summarizer")
	if len(summarizer.Methods) != 1 || summarizer.Methods[0].Name != "Sum" || !summarizer.Methods[0].Signature.Variadic || types.FormatSignature(&artifact.TypeTable, summarizer.Methods[0].Signature) != "function(Int64, variadic Slice<Int64>) Int64" {
		t.Fatalf("expected Summarizer variadic method metadata, got %#v", summarizer.Methods)
	}
}

func artifactTypeByName(t *testing.T, artifact ir.Artifact, name string) types.TypeNode {
	t.Helper()
	declarations := artifact.TypeTable.DefinedNamed(artifact.Module.Path)
	for _, typ := range declarations {
		if typ.Identity.DeclID == types.DeclID(name) {
			return typ
		}
	}
	t.Fatalf("expected type %q in %#v", name, declarations)
	return types.TypeNode{}
}
