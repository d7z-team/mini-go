package cache

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/identity"
	"github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

// The independent traversal detects a schema field added without regenerating
// the typed encoder. Primitive byte order is covered by identity's golden test.
func TestGeneratedIdentityCoversExportedSchema(t *testing.T) {
	data := PackageData{
		ModulePath:       "sample",
		Constants:        []bytecode.Constant{{ID: "large", Value: json.RawMessage(" 9007199254740993 ")}},
		GenericTemplates: []GenericTemplate{{Name: "Map", Decl: ast.Decl{NodeID: 1, Kind: ast.DeclFunc}}},
	}
	for _, tc := range []struct {
		value  any
		encode func(*identity.Writer)
	}{
		{data, func(w *identity.Writer) { EncodeIdentityPackageData(w, data) }},
		{Action{}, func(w *identity.Writer) { EncodeIdentityAction(w, Action{}) }},
		{PrepareAction{}, func(w *identity.Writer) { EncodeIdentityPrepareAction(w, PrepareAction{}) }},
		{SymbolAction{}, func(w *identity.Writer) { EncodeIdentitySymbolAction(w, SymbolAction{}) }},
		{semantic.DependencyPackage{}, func(w *identity.Writer) {
			semantic.EncodeIdentityDependencyPackage(w, semantic.DependencyPackage{})
		}},
		{semantic.DependencyExport{Value: json.RawMessage("9007199254740993")}, func(w *identity.Writer) {
			semantic.EncodeIdentityDependencyExport(w, semantic.DependencyExport{Value: json.RawMessage("9007199254740993")})
		}},
	} {
		t.Run(reflect.TypeOf(tc.value).Name(), func(t *testing.T) {
			actual, reference := identity.New("schema/v1"), identity.New("schema/v1")
			tc.encode(actual)
			var visit func(reflect.Value)
			visit = func(v reflect.Value) {
				if v.Type() == reflect.TypeOf(json.RawMessage{}) {
					reference.JSON(v.Interface().(json.RawMessage))
					return
				}
				switch v.Kind() {
				case reflect.Struct:
					for i := 0; i < v.NumField(); i++ {
						f := v.Type().Field(i)
						if f.IsExported() && f.Tag.Get("json") != "-" {
							if f.Type.Kind() == reflect.Slice && f.Type != reflect.TypeOf(json.RawMessage{}) && !strings.Contains(","+f.Tag.Get("json")+",", ",omitempty,") {
								reference.Bool(!v.Field(i).IsNil())
							}
							visit(v.Field(i))
						}
					}
				case reflect.Pointer:
					reference.Bool(!v.IsNil())
					if !v.IsNil() {
						visit(v.Elem())
					}
				case reflect.Slice, reflect.Array:
					if v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(byte(0)) {
						reference.String(string(v.Bytes()))
						return
					}
					reference.Uint(uint64(v.Len()))
					for i := 0; i < v.Len(); i++ {
						visit(v.Index(i))
					}
				case reflect.String:
					reference.String(v.String())
				case reflect.Bool:
					reference.Bool(v.Bool())
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					reference.Uint(uint64(v.Int()))
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					reference.Uint(v.Uint())
				default:
					t.Fatalf("unhandled identity field %v", v.Type())
				}
			}
			visit(reflect.ValueOf(tc.value))
			a, err := actual.Sum()
			if err != nil {
				t.Fatal(err)
			}
			b, err := reference.Sum()
			if err != nil {
				t.Fatal(err)
			}
			if a != b {
				t.Fatal("generated identity schema is stale")
			}
		})
	}
}

func TestIdentityPreservesRequiredBytesAndOptionalSliceSemantics(t *testing.T) {
	for _, data := range [][]byte{nil, {}, {0, 255, 1}, []byte(strings.Repeat("x", 4097))} {
		actual, reference := identity.New("embed/v1"), identity.New("embed/v1")
		ast.EncodeIdentityEmbedFile(actual, ast.EmbedFile{Path: "asset.bin", Data: data})
		reference.String("asset.bin")
		reference.Bool(data != nil)
		reference.String(string(data))
		got, err := actual.Sum()
		want, wantErr := reference.Sum()
		if err != nil || wantErr != nil || got != want {
			t.Fatalf("embedded bytes %v: %x != %x: %v, %v", data, got, want, err, wantErr)
		}
	}
	first, second := identity.New("expression/v1"), identity.New("expression/v1")
	ast.EncodeIdentityExpression(first, ast.Expression{})
	ast.EncodeIdentityExpression(second, ast.Expression{Args: []ast.Expression{}})
	a, err := first.Sum()
	b, secondErr := second.Sum()
	if err != nil || secondErr != nil || a != b {
		t.Fatalf("omitted empty slice changed identity: %v, %v", err, secondErr)
	}
}

func TestExportIdentityRejectsRecursiveGraphsAndPreservesSharedTrees(t *testing.T) {
	data := PackageData{GenericTemplates: []GenericTemplate{{Decl: ast.Decl{
		Kind: ast.DeclFunc,
		Func: &ast.FuncDecl{Body: ast.BlockStmt{Stmts: []ast.Statement{{Kind: ast.StmtExpr}}}},
	}}}}
	pointerCycle := &ast.Expression{Kind: ast.ExprUnary}
	pointerCycle.Operand = pointerCycle
	sliceCycle := &ast.Expression{Kind: ast.ExprCall, Args: make([]ast.Expression, 1)}
	sliceCycle.Args[0] = *sliceCycle
	for _, expression := range []*ast.Expression{pointerCycle, sliceCycle} {
		data.GenericTemplates[0].Decl.Func.Body.Stmts[0].Expr = expression
		if _, err := data.Hash(); err == nil {
			t.Fatal("cyclic generic AST must not produce an identity")
		}
	}
	leaf := &ast.Expression{Kind: ast.ExprIdent, Name: "value"}
	expression := &ast.Expression{Kind: ast.ExprBinary, Left: leaf, Right: leaf}
	data.GenericTemplates[0].Decl.Func.Body.Stmts[0].Expr = expression
	shared, err := data.Hash()
	if err != nil {
		t.Fatal(err)
	}
	copyOfLeaf := *leaf
	expression.Right = &copyOfLeaf
	copied, err := data.Hash()
	if err != nil || copied != shared {
		t.Fatalf("shared subtree changed identity: %s != %s: %v", copied, shared, err)
	}
}
