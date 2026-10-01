package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func TestLowerCompositeAndSliceExpressions(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	stringType := ast.TypeExpr{Kind: ast.TypeName, Name: "String"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	mapType := ast.TypeExpr{Kind: ast.TypeMap, Key: &stringType, Elem: &intType}
	structType := ast.TypeExpr{
		Kind: ast.TypeStruct,
		Fields: []ast.Field{{
			Name: "Value",
			Type: intType,
		}},
	}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
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
								Names: []string{"xs"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &arrayType,
									Items: []ast.KeyValue{{Value: intLiteral("1")}, {Value: intLiteral("2")}, {Value: intLiteral("3")}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"sliced"},
								Type:  intType,
								Values: []ast.Expression{{
									Kind: ast.ExprIndex,
									Operand: ptrExpr(ast.Expression{
										Kind: ast.ExprSlice,
										Operand: ptrExpr(ast.Expression{
											Kind: ast.ExprIdent,
											Name: "xs",
										}),
										Start: ptrExpr(intLiteral("0")),
										End:   ptrExpr(intLiteral("2")),
									}),
									Index: ptrExpr(intLiteral("1")),
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"mapped"},
								Type:  intType,
								Values: []ast.Expression{{
									Kind: ast.ExprIndex,
									Operand: ptrExpr(ast.Expression{
										Kind: ast.ExprComposite,
										Type: &mapType,
										Items: []ast.KeyValue{{
											Key:   ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "k", Type: &stringType}),
											Value: intLiteral("42"),
										}},
									}),
									Index: ptrExpr(ast.Expression{Kind: ast.ExprLiteral, Literal: "k", Type: &stringType}),
								}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind: ast.ExprSelector,
							Operand: ptrExpr(ast.Expression{
								Kind: ast.ExprComposite,
								Type: &structType,
								Items: []ast.KeyValue{{
									Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Value"}),
									Value: intLiteral("42"),
								}},
							}),
							Field: "Value",
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
	if fn.Body[0].Expr.Kind != ir.ExprSequence {
		t.Fatalf("expected local init array expression, got %#v", fn.Body[0])
	}
	if fn.Body[1].Expr.Kind != ir.ExprLoadIndex || fn.Body[1].Expr.Operand == nil || fn.Body[1].Expr.Operand.Kind != ir.ExprSlice {
		t.Fatalf("expected index of slice expression, got %#v", fn.Body[1].Expr)
	}
	if fn.Body[2].Expr.Kind != ir.ExprLoadIndex || fn.Body[2].Expr.Operand == nil || fn.Body[2].Expr.Operand.Kind != ir.ExprMap {
		t.Fatalf("expected map composite index expression, got %#v", fn.Body[2].Expr)
	}
	if fn.Body[3].Results[0].Kind != ir.ExprLoadField || fn.Body[3].Results[0].Operand == nil || fn.Body[3].Results[0].Operand.Kind != ir.ExprStruct {
		t.Fatalf("expected struct composite member expression, got %#v", fn.Body[3].Results[0])
	}
}

func TestLowerElidedCompositeLiteralTypeFromTarget(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	pointType := ast.TypeExpr{Kind: ast.TypeName, Name: "Point"}
	pointPtrType := ast.TypeExpr{Kind: ast.TypePointer, Elem: &pointType}
	pointsType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &pointType}
	pointPtrsType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &pointPtrType}
	pointStruct := ast.TypeExpr{Kind: ast.TypeStruct, Fields: []ast.Field{{Name: "X", Type: intType}}}
	pointLiteral := func(value string) ast.Expression {
		return ast.Expression{
			Kind: ast.ExprComposite,
			Items: []ast.KeyValue{{
				Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "X"}),
				Value: ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType},
			}},
		}
	}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Point", Type: pointStruct},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"points"},
								Type:  pointsType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &pointsType,
									Items: []ast.KeyValue{{Value: pointLiteral("1")}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"ptrs"},
								Type:  pointPtrsType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &pointPtrsType,
									Items: []ast.KeyValue{{Value: pointLiteral("2")}},
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
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	points := fn.Body[0].Expr
	if points.Kind != ir.ExprSequence || len(points.Elements) != 1 || points.Elements[0].Kind != ir.ExprStruct || hirTypeString(&program.TypeTable, points.Elements[0].Type) != "example/main.Point" {
		t.Fatalf("expected elided slice element to lower as Point struct, got %#v", points)
	}
	ptrs := fn.Body[1].Expr
	if ptrs.Kind != ir.ExprSequence || len(ptrs.Elements) != 1 || ptrs.Elements[0].Kind != ir.ExprLet {
		t.Fatalf("expected elided pointer element to lower through let temp, got %#v", ptrs)
	}
	let := ptrs.Elements[0]
	if let.Bind == nil || let.Bind.Kind != ir.ExprStruct || hirTypeString(&program.TypeTable, let.Bind.Type) != "example/main.Point" || let.Body == nil || let.Body.Kind != ir.ExprAddressOf {
		t.Fatalf("unexpected elided pointer composite emit: %#v", let)
	}
}

func TestLowerKeyedArrayCompositeFillsZeros(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
	arrayType := ast.TypeExpr{Kind: ast.TypeArray, Len: ptrExpr(intLiteral("5")), Elem: &intType}
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
								Names: []string{"xs"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &arrayType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(intLiteral("2")),
										Value: intLiteral("40"),
									}, {
										Key:   ptrExpr(intLiteral("4")),
										Value: intLiteral("2"),
									}},
								}},
							},
						}},
					}, {
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
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	array := fn.Body[0].Expr
	if array.Kind != ir.ExprSequence || len(array.Elements) != 5 {
		t.Fatalf("expected 5-element keyed array expression, got %#v", array)
	}
	if hirTypeString(&program.TypeTable, array.Type) != "Array<5, Int64>" {
		t.Fatalf("expected array type, got %q", array.Type)
	}
	for _, index := range []int{0, 1, 3} {
		if array.Elements[index].Kind != ir.ExprZero || hirTypeString(&program.TypeTable, array.Elements[index].Type) != "Int64" {
			t.Fatalf("expected zero Int64 at index %d, got %#v", index, array.Elements[index])
		}
	}
	if array.Elements[2].Kind != ir.ExprLiteral || string(array.Elements[2].Value.JSON()) != "40" {
		t.Fatalf("expected literal 40 at index 2, got %#v", array.Elements[2])
	}
	if array.Elements[4].Kind != ir.ExprLiteral || string(array.Elements[4].Value.JSON()) != "2" {
		t.Fatalf("expected literal 2 at index 4, got %#v", array.Elements[4])
	}
}

func TestLowerMixedArrayCompositePreservesItemOrder(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
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
								Names: []string{"xs"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &arrayType,
									Items: []ast.KeyValue{{
										Value: intLiteral("1"),
									}, {
										Key:   ptrExpr(intLiteral("3")),
										Value: intLiteral("4"),
									}, {
										Value: intLiteral("5"),
									}},
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
	fn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	array := fn.Body[0].Expr
	if array.Kind != ir.ExprSequence || len(array.Elements) != 5 {
		t.Fatalf("expected 5-element mixed array expression, got %#v", array)
	}
	for index, literal := range map[int]string{0: "1", 3: "4", 4: "5"} {
		if array.Elements[index].Kind != ir.ExprLiteral || string(array.Elements[index].Value.JSON()) != literal {
			t.Fatalf("expected literal %s at index %d, got %#v", literal, index, array.Elements[index])
		}
	}
	for _, index := range []int{1, 2} {
		if array.Elements[index].Kind != ir.ExprZero || hirTypeString(&program.TypeTable, array.Elements[index].Type) != "Int64" {
			t.Fatalf("expected zero Int64 at index %d, got %#v", index, array.Elements[index])
		}
	}
}

func TestLowerSliceShorthandDefaults(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	arrayType := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &intType}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
	identXS := ast.Expression{Kind: ast.ExprIdent, Name: "xs"}
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
								Names: []string{"xs"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:  ast.ExprComposite,
									Type:  &arrayType,
									Items: []ast.KeyValue{{Value: intLiteral("1")}, {Value: intLiteral("2")}, {Value: intLiteral("3")}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"lowDefault"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:    ast.ExprSlice,
									Operand: ptrExpr(identXS),
									End:     ptrExpr(intLiteral("2")),
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"highDefault"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:    ast.ExprSlice,
									Operand: ptrExpr(identXS),
									Start:   ptrExpr(intLiteral("1")),
								}},
							},
						}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{
							Kind: ast.DeclVar,
							Var: &ast.ValueDecl{
								Names: []string{"fullDefault"},
								Type:  arrayType,
								Values: []ast.Expression{{
									Kind:    ast.ExprSlice,
									Operand: ptrExpr(identXS),
								}},
							},
						}},
					}, {
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{intLiteral("0")},
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
	lowDefault := fn.Body[1].Expr
	if lowDefault.Kind != ir.ExprSlice || lowDefault.Start == nil || lowDefault.Start.Kind != ir.ExprLiteral || string(lowDefault.Start.Value.JSON()) != "0" {
		t.Fatalf("expected missing low to default to zero literal, got %#v", lowDefault)
	}
	highDefault := fn.Body[2].Expr
	if highDefault.Kind != ir.ExprLet || highDefault.Body == nil || highDefault.Body.Kind != ir.ExprSlice {
		t.Fatalf("expected missing high to lower through let-bound slice, got %#v", highDefault)
	}
	if highDefault.Body.End == nil || highDefault.Body.End.Kind != ir.ExprLen {
		t.Fatalf("expected missing high to default to len(object), got %#v", highDefault.Body)
	}
	fullDefault := fn.Body[3].Expr
	if fullDefault.Kind != ir.ExprLet || fullDefault.Body == nil || fullDefault.Body.Start == nil ||
		fullDefault.Body.Start.Kind != ir.ExprLiteral || string(fullDefault.Body.Start.Value.JSON()) != "0" ||
		fullDefault.Body.End == nil || fullDefault.Body.End.Kind != ir.ExprLen {
		t.Fatalf("expected full slice shorthand to default low/high, got %#v", fullDefault)
	}
}
