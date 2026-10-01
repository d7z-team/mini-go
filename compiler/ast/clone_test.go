package ast

import (
	"encoding/json"
	"testing"
)

func TestCloneDeclarationOwnsPayloadAndRetainsSparseWireShape(t *testing.T) {
	declarations := []Decl{
		{Kind: DeclImport, Import: &ImportDecl{Path: "original"}},
		{Kind: DeclConst, Const: &ValueDecl{Names: []string{"original"}}},
		{Kind: DeclVar, Var: &ValueDecl{Names: []string{"original"}}},
		{Kind: DeclType, Type: &TypeDecl{Name: "original"}},
		{Kind: DeclFunc, Func: &FuncDecl{Name: "original"}},
	}
	for _, declaration := range declarations {
		t.Run(string(declaration.Kind), func(t *testing.T) {
			clone := CloneDecl(declaration)
			switch clone.Kind {
			case DeclImport:
				clone.Import.Path = "changed"
			case DeclConst:
				clone.Const.Names[0] = "changed"
			case DeclVar:
				clone.Var.Names[0] = "changed"
			case DeclType:
				clone.Type.Name = "changed"
			case DeclFunc:
				clone.Func.Name = "changed"
			}
			data, err := json.Marshal(declaration)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for _, kind := range []DeclKind{DeclImport, DeclConst, DeclVar, DeclType, DeclFunc} {
				_, present := fields[string(kind)]
				if present != (kind == declaration.Kind) {
					t.Fatalf("unexpected payload %q in %s", kind, data)
				}
			}
			var restored Decl
			if err = json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			switch restored.Kind {
			case DeclImport:
				if restored.Import.Path != "original" {
					t.Fatal("shared import payload")
				}
			case DeclConst:
				if restored.Const.Names[0] != "original" {
					t.Fatal("shared const payload")
				}
			case DeclVar:
				if restored.Var.Names[0] != "original" {
					t.Fatal("shared var payload")
				}
			case DeclType:
				if restored.Type.Name != "original" {
					t.Fatal("shared type payload")
				}
			case DeclFunc:
				if restored.Func.Name != "original" {
					t.Fatal("shared func payload")
				}
			}
		})
	}
}

func TestCloneProgramDoesNotShareMutableNodes(t *testing.T) {
	program := Program{Files: []File{{Decls: []Decl{{Kind: DeclFunc, Func: &FuncDecl{
		Name:   "F",
		Params: []Field{{Name: "values", Type: TypeExpr{Kind: TypeSlice, Elem: &TypeExpr{Kind: TypeName, Name: "int"}}}},
		Body: BlockStmt{Stmts: []Statement{{Kind: StmtReturn, Results: []Expression{{
			Kind:  ExprComposite,
			Type:  &TypeExpr{Kind: TypeSlice, Elem: &TypeExpr{Kind: TypeName, Name: "int"}},
			Items: []KeyValue{{Value: Expression{Kind: ExprLiteral, Literal: "1"}}},
		}, {
			Kind: ExprFunc,
			Func: &FuncDecl{Body: BlockStmt{Stmts: []Statement{{Kind: StmtReturn, Results: []Expression{{Kind: ExprLiteral, Literal: "3"}}}}}},
		}}}}},
	}}}}}}

	clone := CloneProgram(program)
	clone.Files[0].Decls[0].Func.Params[0].Type.Elem.Name = "string"
	clone.Files[0].Decls[0].Func.Body.Stmts[0].Results[0].Type.Elem.Name = "string"
	clone.Files[0].Decls[0].Func.Body.Stmts[0].Results[0].Items[0].Value.Literal = "2"
	clone.Files[0].Decls[0].Func.Body.Stmts[0].Results[1].Func.Body.Stmts[0].Results[0].Literal = "4"

	function := program.Files[0].Decls[0].Func
	if function.Body.Stmts[0].Results[0].Type.Elem.Name != "int" {
		t.Fatal("cloned composite shares its type with the source")
	}
	if clone.Files[0].Decls[0].Func.Body.Stmts[0].Results[1].Type != nil {
		t.Fatal("clone materialized a type for an inferred expression")
	}
	if function.Params[0].Type.Elem.Name != "int" {
		t.Fatalf("source parameter type changed to %q", function.Params[0].Type.Elem.Name)
	}
	if literal := function.Body.Stmts[0].Results[0].Items[0].Value.Literal; literal != "1" {
		t.Fatalf("source literal changed to %q", literal)
	}
	if literal := function.Body.Stmts[0].Results[1].Func.Body.Stmts[0].Results[0].Literal; literal != "3" {
		t.Fatalf("source function literal changed to %q", literal)
	}
}
