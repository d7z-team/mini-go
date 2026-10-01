package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerTopLevelIntConstExpression(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Shift"},
					Type:   intType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "5", Type: &intType}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Mask"},
					Type:  intType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: "<<",
						Left:     &ast.Expression{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
						Right:    &ast.Expression{Kind: ast.ExprIdent, Name: "Shift"},
					}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "Mask"}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.Constants) != 2 || program.Constants[1].ID != "const.Mask" || hirTypeString(&program.TypeTable, program.Constants[1].Type) != "Int64" || string(program.Constants[1].Value.JSON()) != "32" {
		t.Fatalf("expected folded Int64 constant expression, got %#v", program.Constants)
	}
}

func TestLowerTopLevelStringConstExpression(t *testing.T) {
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Prefix"},
					Type:   stringType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: `"mini"`, Type: &stringType}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Name"},
					Type:  stringType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: "+",
						Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "Prefix"},
						Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: `"-go"`, Type: &stringType},
					}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: stringType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprIdent, Name: "Name"}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	if len(program.Constants) != 2 || program.Constants[1].ID != "const.Name" || hirTypeString(&program.TypeTable, program.Constants[1].Type) != "String" || string(program.Constants[1].Value.JSON()) != `"mini-go"` {
		t.Fatalf("expected folded String constant expression, got %#v", program.Constants)
	}
}

func TestLowerTopLevelFloatConstExpression(t *testing.T) {
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	floatType := ast.TypeExpr{Kind: ast.TypeName, Name: "Float64"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Half"},
					Type:   floatType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "1.5", Type: &floatType}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Total"},
					Type:  floatType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: "+",
						Left: &ast.Expression{
							Kind:     ast.ExprBinary,
							Operator: "*",
							Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "Half"},
							Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType},
						},
						Right: &ast.Expression{Kind: ast.ExprLiteral, Literal: "39.5", Type: &floatType},
					}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Large"},
					Type:  boolType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: ">",
						Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "Total"},
						Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: "42", Type: &intType},
					}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	values := map[string]string{}
	types := map[string]string{}
	for _, constant := range program.Constants {
		values[constant.Name] = string(constant.Value.JSON())
		types[constant.Name] = hirTypeString(&program.TypeTable, constant.Type)
	}
	if types["Total"] != "Float64" || values["Total"] != "42.5" {
		t.Fatalf("expected folded Float64 total, got %#v", program.Constants)
	}
	if types["Large"] != "Bool" || values["Large"] != "true" {
		t.Fatalf("expected folded Float64 comparison, got %#v", program.Constants)
	}
}

func TestLowerTopLevelBoolConstExpression(t *testing.T) {
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names:  []string{"Enabled"},
					Type:   boolType,
					Values: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "true", Type: &boolType}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Gate"},
					Type:  boolType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: "&&",
						Left:     &ast.Expression{Kind: ast.ExprIdent, Name: "Enabled"},
						Right: &ast.Expression{
							Kind:     ast.ExprUnary,
							Operator: "!",
							Operand:  &ast.Expression{Kind: ast.ExprLiteral, Literal: "false", Type: &boolType},
						},
					}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Small"},
					Type:  boolType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: "<",
						Left:     &ast.Expression{Kind: ast.ExprLiteral, Literal: "5", Type: &intType},
						Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: "6", Type: &intType},
					}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Ordered"},
					Type:  boolType,
					Values: []ast.Expression{{
						Kind:     ast.ExprBinary,
						Operator: "<",
						Left:     &ast.Expression{Kind: ast.ExprLiteral, Literal: `"a"`, Type: &stringType},
						Right:    &ast.Expression{Kind: ast.ExprLiteral, Literal: `"b"`, Type: &stringType},
					}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	values := map[string]string{}
	for _, constant := range program.Constants {
		values[constant.Name] = string(constant.Value.JSON())
		if hirTypeString(&program.TypeTable, constant.Type) != "Bool" {
			t.Fatalf("expected Bool constant, got %#v", constant)
		}
	}
	for _, name := range []string{"Gate", "Small", "Ordered"} {
		if values[name] != "true" {
			t.Fatalf("expected %s to fold to true, got constants %#v", name, program.Constants)
		}
	}
}

func TestLowerTopLevelConstConversions(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	runeType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int32"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Counter", Type: intType},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Base"},
					Values: []ast.Expression{{
						Kind:    ast.ExprConvert,
						Type:    &intType,
						Operand: ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "40", Type: &intType}),
					}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Bonus"},
					Type:  counterType,
					Values: []ast.Expression{{
						Kind:    ast.ExprConvert,
						Type:    &counterType,
						Operand: ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "2", Type: &intType}),
					}},
				},
			}, {
				Kind: ast.DeclConst,
				Const: &ast.ValueDecl{
					Names: []string{"Letter"},
					Type:  stringType,
					Values: []ast.Expression{{
						Kind:    ast.ExprConvert,
						Type:    &stringType,
						Operand: ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "65", Type: &runeType}),
					}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	values := map[string]string{}
	types := map[string]string{}
	for _, constant := range program.Constants {
		values[constant.Name] = string(constant.Value.JSON())
		types[constant.Name] = hirTypeString(&program.TypeTable, constant.Type)
	}
	if types["Base"] != "Int64" || values["Base"] != "40" {
		t.Fatalf("expected Base Int64 conversion, got %#v", program.Constants)
	}
	if types["Bonus"] != "example/main.Counter" || values["Bonus"] != "2" {
		t.Fatalf("expected Bonus Counter conversion, got %#v", program.Constants)
	}
	if types["Letter"] != "String" || values["Letter"] != `"A"` {
		t.Fatalf("expected Letter String conversion, got %#v", program.Constants)
	}
}

func TestLowerBuiltinTypeCallConversion(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	runeType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int32"}
	boolType := ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:    "Main",
					Results: []ast.Field{{Type: boolType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:     ast.ExprBinary,
							Operator: "==",
							Left: ptrExpr(ast.Expression{
								Kind:   ast.ExprCall,
								Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "rune"}),
								Args: []ast.Expression{{
									Kind:    ast.ExprLiteral,
									Literal: "128512",
									Type:    &intType,
								}},
							}),
							Right: ptrExpr(ast.Expression{
								Kind:    ast.ExprLiteral,
								Literal: "128512",
								Type:    &runeType,
							}),
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
	result := fn.Body[0].Results[0]
	if result.Kind != ir.ExprLiteral || hirTypeString(&program.TypeTable, result.Type) != "Bool" || string(result.Value.JSON()) != "true" {
		t.Fatalf("expected rune conversion comparison to fold to true, got %#v", result)
	}
}

func TestLowerNilConversionInsideBuiltinCall(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	sliceType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	nilSlice := ast.Expression{
		Kind: ast.ExprConvert,
		Type: &sliceType,
		Operand: ptrExpr(ast.Expression{
			Kind:    ast.ExprLiteral,
			Literal: "nil",
			Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: "Any"},
		}),
	}
	appendCall := ast.Expression{
		Kind:   ast.ExprCall,
		Callee: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "append"}),
		Args: []ast.Expression{
			nilSlice,
			{Kind: ast.ExprLiteral, Literal: "1", Type: &intType},
		},
	}
	file := ast.File{
		Path: "main.mgo",
		Decls: []ast.Decl{{
			Kind: ast.DeclFunc,
			Func: &ast.FuncDecl{
				Name:    "Main",
				Results: []ast.Field{{Type: sliceType}},
				Body: ast.BlockStmt{Stmts: []ast.Statement{{
					Kind:    ast.StmtReturn,
					Results: []ast.Expression{appendCall},
				}}},
			},
		}},
	}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files:      []ast.File{file},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok || len(fn.Body) == 0 || len(fn.Body[0].Results) != 1 {
		t.Fatalf("expected Main return expression, got %#v", program.Functions)
	}
	appendExpr := fn.Body[0].Results[0]
	if appendExpr.Kind != ir.ExprAppend || appendExpr.Operand == nil {
		t.Fatalf("expected append expression, got %#v", appendExpr)
	}
	if appendExpr.Operand.Kind != ir.ExprLiteral || hirTypeString(&program.TypeTable, appendExpr.Operand.Type) != "Slice<Int>" || string(appendExpr.Operand.Value.JSON()) != "null" {
		t.Fatalf("expected typed nil slice operand, got %#v", appendExpr.Operand)
	}
}

func TestLowerNamedFunctionNilConversion(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int"}
	functionType := ast.TypeExpr{Kind: ast.TypeFunc, Results: []ast.Field{{Type: intType}}}
	handlerType := ast.TypeExpr{Kind: ast.TypeName, Name: "Handler"}
	nilHandler := ast.Expression{
		Kind: ast.ExprConvert,
		Type: &handlerType,
		Operand: ptrExpr(ast.Expression{
			Kind:    ast.ExprLiteral,
			Literal: "nil",
			Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: "Any"},
		}),
	}
	file := ast.File{
		Path: "main.mgo",
		Decls: []ast.Decl{
			{Kind: ast.DeclType, Type: &ast.TypeDecl{Name: "Handler", Type: functionType}},
			{Kind: ast.DeclFunc, Func: &ast.FuncDecl{
				Name:    "Main",
				Results: []ast.Field{{Type: handlerType}},
				Body: ast.BlockStmt{Stmts: []ast.Statement{{
					Kind:    ast.StmtReturn,
					Results: []ast.Expression{nilHandler},
				}}},
			}},
		},
	}
	program, diagnostics := lowerTestProgram(ast.Program{ModulePath: "example/main", Package: "main", Files: []ast.File{file}})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok || len(fn.Body) == 0 || len(fn.Body[0].Results) != 1 {
		t.Fatalf("expected Main return expression, got %#v", program.Functions)
	}
	result := fn.Body[0].Results[0]
	if result.Kind != ir.ExprLiteral || hirTypeString(&program.TypeTable, result.Type) != "example/main.Handler" || string(result.Value.JSON()) != "null" {
		t.Fatalf("expected typed nil named function value, got %#v", result)
	}
}
