package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestLowerReceiverPreservesNamedIdentity(t *testing.T) {
	for _, name := range []string{"Error", "String", "Int"} {
		t.Run(name, func(t *testing.T) {
			receiver := ast.TypeExpr{Kind: ast.TypePointer, Elem: &ast.TypeExpr{Kind: ast.TypeName, Name: name}}
			program, diagnostics := lowerTestProgram(ast.Program{
				ModulePath: "example/main", Package: "main",
				Files: []ast.File{{Path: "main.mgo", Decls: []ast.Decl{
					{Kind: ast.DeclType, Type: &ast.TypeDecl{Name: name, Type: ast.TypeExpr{Kind: ast.TypeStruct}}},
					{Kind: ast.DeclFunc, Func: &ast.FuncDecl{Name: "Touch", Receiver: &ast.Field{Name: "value", Type: receiver}}},
				}}},
			})
			if len(diagnostics) != 0 {
				t.Fatalf("lower: %v", diagnostics)
			}
			for _, function := range program.Functions {
				if len(function.Signature.Params) == 0 {
					continue
				}
				local := function.Locals[0].Type
				if !types.NewRelations(&program.TypeTable).Identical(local, function.Signature.Params[0].Type).OK {
					t.Fatalf("receiver local %v differs from signature %v", local, function.Signature.Params[0].Type)
				}
				node, ok := program.TypeTable.Node(local)
				if !ok || node.Kind != types.Pointer || node.Elem.Kind != types.Named || node.Elem.Named.DeclID != types.DeclID(name) {
					t.Fatalf("receiver lost named identity: %#v", node)
				}
				return
			}
			t.Fatal("method was not lowered")
		})
	}
}

func TestLowerMethodDeclarationAndSelectorCall(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	counterStruct := ast.TypeExpr{Kind: ast.TypeStruct, Fields: []ast.Field{{Name: "Value", Type: intType}}}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Counter", Type: counterStruct},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:     "Add",
					Receiver: &ast.Field{Name: "c", Type: counterType},
					Params:   []ast.Field{{Name: "delta", Type: intType}},
					Results:  []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:     ast.ExprBinary,
							Operator: "+",
							Left: &ast.Expression{
								Kind:    ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "c"}),
								Field:   "Value",
							},
							Right: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "delta"}),
						}},
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
								Names: []string{"c"},
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &counterType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Value"}),
										Value: intLiteral("40"),
									}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind: ast.ExprCall,
							Callee: ptrExpr(ast.Expression{
								Kind:    ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "c"}),
								Field:   "Add",
							}),
							Args: []ast.Expression{intLiteral("2")},
						}},
					}}},
				},
			}},
		}},
	})
	if len(diagnostics) != 0 {
		t.Fatalf("Lower returned diagnostics: %#v", diagnostics)
	}
	method, ok := findFunction(program.Functions, "method.Counter.Add")
	if !ok {
		t.Fatalf("expected method function, got %#v", program.Functions)
	}
	if types.FormatSignature(&program.TypeTable, method.Signature) != "function(example/main.Counter, Int64) Int64" || len(method.Locals) < 2 ||
		method.Locals[0].Name != "c" || method.Locals[1].Name != "delta" {
		t.Fatalf("unexpected lowered method: %#v", method)
	}
	mainFn, ok := findFunction(program.Functions, "fn.Main")
	if !ok {
		t.Fatalf("expected Main function, got %#v", program.Functions)
	}
	call := mainFn.Body[1].Results[0]
	if call.Kind != hir.ExprCallDirect || call.Function != "method.Counter.Add" || len(call.Args) != 2 {
		t.Fatalf("expected selector method direct call with receiver arg, got %#v", call)
	}
}

func TestLowerAutoAddressPointerReceiverMethodCall(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	counterPtrType := ast.TypeExpr{Kind: ast.TypePointer, Elem: &counterType}
	counterStruct := ast.TypeExpr{Kind: ast.TypeStruct, Fields: []ast.Field{{Name: "Value", Type: intType}}}
	intLiteral := func(value string) ast.Expression {
		return ast.Expression{Kind: ast.ExprLiteral, Literal: value, Type: &intType}
	}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Counter", Type: counterStruct},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:     "Inc",
					Receiver: &ast.Field{Name: "c", Type: counterPtrType},
					Params:   []ast.Field{{Name: "delta", Type: intType}},
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
								Names: []string{"c"},
								Values: []ast.Expression{{
									Kind: ast.ExprComposite,
									Type: &counterType,
									Items: []ast.KeyValue{{
										Key:   ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Value"}),
										Value: intLiteral("40"),
									}},
								}},
							},
						}},
					}, {
						Kind: ast.StmtExpr,
						Expr: &ast.Expression{
							Kind: ast.ExprCall,
							Callee: ptrExpr(ast.Expression{
								Kind:    ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "c"}),
								Field:   "Inc",
							}),
							Args: []ast.Expression{intLiteral("2")},
						},
					}, {
						Kind: ast.StmtReturn,
						Results: []ast.Expression{{
							Kind:    ast.ExprSelector,
							Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "c"}),
							Field:   "Value",
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
	call := mainFn.Body[1].Expr
	if call.Kind != hir.ExprCallDirect || call.Function != "method.Ptr<Counter>.Inc" || len(call.Args) != 2 {
		t.Fatalf("expected pointer receiver direct call, got %#v", call)
	}
	if call.Args[0].Kind != hir.ExprAddressOf || call.Args[0].Local == "" {
		t.Fatalf("expected addressable local receiver arg, got %#v", call.Args[0])
	}
}

func TestLowerMethodValueCreatesWrapperFunction(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	counterStruct := ast.TypeExpr{Kind: ast.TypeStruct, Fields: []ast.Field{{Name: "Value", Type: intType}}}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Counter", Type: counterStruct},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:     "Add",
					Receiver: &ast.Field{Name: "c", Type: counterType},
					Params:   []ast.Field{{Name: "delta", Type: intType}},
					Results:  []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"c"},
							Type:  counterType,
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"f"},
							Values: []ast.Expression{{
								Kind:    ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "c"}),
								Field:   "Add",
							}},
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
	if len(mainFn.Body) != 2 || mainFn.Body[0].Kind != hir.StmtStoreLocal || !mainFn.Body[0].Rebind ||
		mainFn.Body[1].Expr.Kind != hir.ExprLet || mainFn.Body[1].Expr.Body == nil || mainFn.Body[1].Expr.Body.Kind != hir.ExprFunction {
		t.Fatalf("expected method value initializer to lower through let function value, got %#v", mainFn.Body)
	}
	fType := ""
	for _, local := range mainFn.Locals {
		if local.Name == "f" {
			fType = hirTypeString(&program.TypeTable, local.Type)
			break
		}
	}
	if fType != "function(Int64) Int64" {
		t.Fatalf("expected f local to keep method value signature, got %#v", mainFn.Locals)
	}
	wrapperID := mainFn.Body[1].Expr.Body.Function
	wrapper, ok := findFunction(program.Functions, wrapperID)
	if !ok {
		t.Fatalf("expected method value wrapper %s, got %#v", wrapperID, program.Functions)
	}
	if types.FormatSignature(&program.TypeTable, wrapper.Signature) != "function(Int64) Int64" || len(wrapper.Upvalues) != 1 || hirTypeString(&program.TypeTable, wrapper.Upvalues[0].Type) != "example/main.Counter" {
		t.Fatalf("unexpected method value wrapper: %#v", wrapper)
	}
	if len(wrapper.Body) != 1 || len(wrapper.Body[0].Results) != 1 || wrapper.Body[0].Results[0].Function != "method.Counter.Add" {
		t.Fatalf("expected wrapper to direct-call method, got %#v", wrapper.Body)
	}
}

func TestLowerMethodExpressionCreatesFunctionValue(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "Int64"}
	counterType := ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}
	counterPtrType := ast.TypeExpr{Kind: ast.TypePointer, Elem: &counterType}
	counterStruct := ast.TypeExpr{Kind: ast.TypeStruct, Fields: []ast.Field{{Name: "Value", Type: intType}}}
	program, diagnostics := lowerTestProgram(ast.Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []ast.File{{
			Path: "main.mgo",
			Decls: []ast.Decl{{
				Kind: ast.DeclType,
				Type: &ast.TypeDecl{Name: "Counter", Type: counterStruct},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:     "Add",
					Receiver: &ast.Field{Name: "c", Type: counterType},
					Params:   []ast.Field{{Name: "delta", Type: intType}},
					Results:  []ast.Field{{Type: intType}},
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind:    ast.StmtReturn,
						Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: "0", Type: &intType}},
					}}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name:     "Inc",
					Receiver: &ast.Field{Name: "c", Type: counterPtrType},
					Params:   []ast.Field{{Name: "delta", Type: intType}},
				},
			}, {
				Kind: ast.DeclFunc,
				Func: &ast.FuncDecl{
					Name: "Main",
					Body: ast.BlockStmt{Stmts: []ast.Statement{{
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"f"},
							Values: []ast.Expression{{
								Kind:    ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Counter"}),
								Field:   "Add",
							}},
						}}},
					}, {
						Kind: ast.StmtDecl,
						Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
							Names: []string{"g"},
							Values: []ast.Expression{{
								Kind: ast.ExprSelector,
								Operand: ptrExpr(ast.Expression{
									Kind:    ast.ExprDeref,
									Operand: ptrExpr(ast.Expression{Kind: ast.ExprIdent, Name: "Counter"}),
								}),
								Field: "Inc",
							}},
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
	if len(mainFn.Locals) != 2 || hirTypeString(&program.TypeTable, mainFn.Locals[0].Type) != "function(example/main.Counter, Int64) Int64" || hirTypeString(&program.TypeTable, mainFn.Locals[1].Type) != "function(Ptr<example/main.Counter>, Int64)" {
		t.Fatalf("expected method expression locals to keep concrete signatures, got %#v", mainFn.Locals)
	}
	if len(mainFn.Body) != 2 {
		t.Fatalf("expected two method expression stores, got %#v", mainFn.Body)
	}
	first := mainFn.Body[0]
	if first.Kind != hir.StmtStoreResults || first.Expr.Kind != hir.ExprFunction || first.Expr.Function != "method.Counter.Add" {
		t.Fatalf("expected Counter.Add method expression to become direct function value, got %#v", first)
	}
	second := mainFn.Body[1]
	if second.Kind != hir.StmtStoreResults || second.Expr.Kind != hir.ExprFunction || second.Expr.Function != "method.Ptr<Counter>.Inc" {
		t.Fatalf("expected (*Counter).Inc method expression to become direct function value, got %#v", second)
	}
}
