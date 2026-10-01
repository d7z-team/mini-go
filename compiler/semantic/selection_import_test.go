package semantic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestRecursiveGenericEmbeddingKeepsConcreteFieldSelection(t *testing.T) {
	parsed := parser.ParseSource("example", "main.mgo", `package main
type Nest[T any] struct { *Nest[*T]; Value T }
func Main() { var value Nest[int]; _ = value.Value; _ = value.Missing }
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatal(parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	found := false
	for _, selection := range checked.Info.Selections {
		if selection.Name == "Value" {
			found = true
			if selection.Type != types.Builtin(types.PrimitiveInt) {
				t.Fatalf("recursive field type: %+v", selection.Type)
			}
		}
		if selection.Name == "Missing" {
			t.Fatal("unresolved member acquired a selection")
		}
	}
	if !found {
		t.Fatal("concrete field selection missing")
	}
}

func TestRepeatedDiamondRetainsFieldAmbiguity(t *testing.T) {
	var source strings.Builder
	source.WriteString("package main\ntype A0 struct{X int}\ntype B0 struct{X int}\n")
	for depth := 1; depth <= 30; depth++ {
		fmt.Fprintf(&source, "type A%d struct{A%d;B%d}\ntype B%d struct{A%d;B%d}\n", depth, depth-1, depth-1, depth, depth-1, depth-1)
	}
	source.WriteString("func Main(){_ = A30{X:1}}\n")
	parsed := parser.ParseSource("example/main", "main.mgo", source.String())
	if len(parsed.Diagnostics) != 0 {
		t.Fatal(parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	for _, diagnostic := range checked.Info.Diagnostics {
		if diagnostic.Code == "semantic.composite.ambiguous" {
			return
		}
	}
	t.Fatalf("missing diamond ambiguity: %+v", checked.Info.Diagnostics)
}

func TestImportedPromotedMethodPreservesReceiverPath(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main
import "example/lib"
func Run(value *lib.Outer) int { return value.Read() }
`)
	method := DependencyTypeMethod{
		Name: "Read", Receiver: "Ptr<example/lib.Inner>", Signature: "function() Int",
		FunctionID: "method.Ptr<Inner>.Read", ModulePath: "example/lib",
	}
	checked := WithOptions(parsed.Program, AnalyzeOptions{Dependencies: testDependencies([]DependencyExport{
		{
			ModulePath: "example/lib", Name: "Inner", Kind: ObjectType, Type: "example/lib.Inner",
			Underlying: "struct{Value:Int}", Fields: []DependencyTypeField{{Name: "Value", Type: "Int"}}, Methods: []DependencyTypeMethod{method},
		},
		{
			ModulePath: "example/lib", Name: "Outer", Kind: ObjectType, Type: "example/lib.Outer",
			Underlying: "struct{embedded Inner:Ptr<example/lib.Inner>}",
			Fields:     []DependencyTypeField{{Name: "Inner", Type: "Ptr<example/lib.Inner>", Embedded: true}}, Methods: []DependencyTypeMethod{method},
		},
	})})
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatal(checked.Info.Diagnostics)
	}
	found := false
	for _, selection := range checked.Info.Selections {
		if selection.Kind == SelectionMethod && selection.Name == "Read" {
			found = true
			if len(selection.Index) != 1 || selection.Index[0] != 0 {
				t.Fatalf("path = %v", selection.Index)
			}
		}
	}
	if !found {
		t.Fatal("missing method selection")
	}
}
