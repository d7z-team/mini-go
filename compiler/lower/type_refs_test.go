package lower

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestLowererTypeRefsNormalizeCanonicalText(t *testing.T) {
	l := lowerer{modulePath: "example/main"}
	first, ok := l.typeRef("Slice<int64>")
	if !ok {
		t.Fatal("expected slice type to parse")
	}
	second, ok := l.typeRef("Slice<Int64>")
	if !ok {
		t.Fatal("expected normalized slice type to parse")
	}
	if !first.Equal(second) {
		t.Fatalf("normalized type refs differ: first=%#v second=%#v", first, second)
	}
	view, ok := l.typeView("Slice<int64>")
	if !ok || view.Shape() != types.Slice {
		t.Fatalf("slice view = %#v ok=%v", view, ok)
	}
	elem, ok := view.Elem()
	if !ok || elem != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("slice elem = %#v ok=%v", elem, ok)
	}
}

func TestLowererClassifiesNumericTypes(t *testing.T) {
	l := lowerer{modulePath: "example/main"}
	if !l.isIntegerType("int32") || !l.isSignedIntegerType("int32") || !l.isUnsignedIntegerType("uint8") || !l.isFloatType("float32") || !l.isComplexType("complex128") {
		t.Fatal("expected source numeric aliases to normalize before classification")
	}
	if l.isIntegerType("String") {
		t.Fatal("string must not classify as integer")
	}
}

func TestLowererRelationsClassifyMapKeys(t *testing.T) {
	l := lowerer{modulePath: "example/main"}
	arrayRef, ok := l.typeRef("Array<2, Int64>")
	if !ok {
		t.Fatal("expected array type to parse")
	}
	if got := l.typeRelations().MapKeyAllowed(arrayRef); !got.OK {
		t.Fatalf("array of comparable elements should be a valid map key: %#v", got)
	}
	sliceRef, ok := l.typeRef("Slice<Int64>")
	if !ok {
		t.Fatal("expected slice type to parse")
	}
	if got := l.typeRelations().MapKeyAllowed(sliceRef); got.OK || got.Code != types.RelationNotComparable {
		t.Fatalf("slice should not be a valid map key: %#v", got)
	}
}

func TestLowererRegistersDependencyTypeCatalog(t *testing.T) {
	l := lowerer{
		modulePath:    "example/main",
		moduleExports: map[string]map[string]moduleExportInfo{},
	}
	l.collectDependencyExports([]check.DependencyExport{
		{
			ModulePath: "example/lib",
			Name:       "Alias",
			Kind:       check.ObjectType,
			Type:       "Int64",
		},
		{
			ModulePath: "example/lib",
			Name:       "Point",
			Kind:       check.ObjectType,
			Type:       "example/lib.Point",
			Underlying: "struct{X:Int64,Y:String}",
			Methods: []check.DependencyTypeMethod{{
				Name:       "Read",
				Receiver:   "example/lib.Point",
				Signature:  "function() Int64",
				FunctionID: "method.Point.Read",
				ModulePath: "example/lib",
			}},
		},
		{
			ModulePath: "example/lib",
			Name:       "Reader",
			Kind:       check.ObjectType,
			Type:       "example/lib.Reader",
			Underlying: "interface{Read:function() Int64}",
			Methods: []check.DependencyTypeMethod{{
				Name:       "Read",
				Signature:  "function() Int64",
				ModulePath: "example/lib",
			}},
		},
	})
	aliasRef, ok := l.typeRef("example/lib.Alias")
	if !ok {
		t.Fatal("expected imported alias type to parse")
	}
	if view := types.View(l.typeTable, aliasRef); !view.IsAlias() || view.Shape() != types.Primitive {
		t.Fatalf("expected imported alias to resolve through TypeTable, got %#v", view)
	}
	pointRef, ok := l.typeRef("example/lib.Point")
	if !ok {
		t.Fatal("expected imported defined type to parse")
	}
	fields, ok := types.View(l.typeTable, pointRef).StructFields()
	if !ok || len(fields) != 2 || fields[0].Name != "X" || fields[1].Name != "Y" {
		t.Fatalf("expected imported struct fields from dependency catalog, got %#v ok=%v", fields, ok)
	}
	readerRef, ok := l.typeRef("example/lib.Reader")
	if !ok {
		t.Fatal("expected imported interface type to parse")
	}
	if result := l.typeRelations().Implements(pointRef, readerRef); !result.OK {
		t.Fatalf("expected imported method metadata to satisfy interface: %#v", result)
	}
}

func TestLowererClassifiesNilAssignableTypes(t *testing.T) {
	l := lowerer{modulePath: "example/main"}
	for _, typ := range []string{
		"Ptr<Int64>",
		"Slice<Int64>",
		"Map<String, Int64>",
		"function() Int64",
		"ReceiveWaitable<Int64>",
	} {
		if !l.isNilAssignableType(typ) {
			t.Fatalf("%s should accept nil", typ)
		}
	}
	for _, typ := range []string{"Int64", "struct{A:Int64}"} {
		if l.isNilAssignableType(typ) {
			t.Fatalf("%s should not accept nil", typ)
		}
	}
}

func TestLowererPreservesChannelTypeSemantics(t *testing.T) {
	intType := ast.TypeExpr{Kind: ast.TypeName, Name: "int64"}
	l := lowerer{
		modulePath: "example/main",
		typeDecls: map[string]ast.TypeExpr{
			"Counter": {Kind: ast.TypeName, Name: "int"},
			"Both":    {Kind: ast.TypeChan, Elem: &intType},
			"Recv":    {Kind: ast.TypeChan, Direction: "recv", Elem: &intType},
			"Send":    {Kind: ast.TypeChan, Direction: "send", Elem: &intType},
		},
	}
	if !l.isChannelType("Both") || !l.isChannelType("Recv") || !l.isChannelType("Send") {
		t.Fatal("named waitable types should be recognized")
	}
	if got := l.channelElementType("ReceiveWaitable<int64>"); got != "Int64" {
		t.Fatalf("receive waitable elem = %q, want Int64", got)
	}
	if got := l.channelElementType("Both"); got != "Int64" {
		t.Fatalf("named waitable elem = %q, want Int64", got)
	}
	if got := l.channelElementType("Waitable<Counter>"); got != "Counter" {
		t.Fatalf("local named waitable elem = %q, want Counter", got)
	}
	info, ok := l.channelTypeInfo("SendWaitable<Int64>")
	if !ok || info.direction != "send" || info.elem != "Int64" {
		t.Fatalf("send waitable info = %#v ok=%v", info, ok)
	}
	if !l.channelAssignableType("Waitable<Int64>", "ReceiveWaitable<Int64>") {
		t.Fatal("bidirectional waitable should assign to receive waitable")
	}
	if l.channelAssignableType("ReceiveWaitable<Int64>", "Waitable<Int64>") {
		t.Fatal("receive waitable should not assign to bidirectional waitable")
	}
	if l.channelAssignableType("Both", "Recv") {
		t.Fatal("distinct named waitable types must preserve namedness assignment rule")
	}
}

func TestLowererPreservesArrayAndSliceTypeSemantics(t *testing.T) {
	l := lowerer{
		modulePath: "example/main",
		typeDecls: map[string]ast.TypeExpr{
			"Counter": {Kind: ast.TypeName, Name: "int"},
			"Vector": {Kind: ast.TypeArray, Len: &ast.Expression{
				Kind: ast.ExprLiteral, Literal: "2", Type: &ast.TypeExpr{Kind: ast.TypeName, Name: "Int"},
			}, Elem: &ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}},
			"Scores": {Kind: ast.TypeSlice, Elem: &ast.TypeExpr{Kind: ast.TypeName, Name: "Counter"}},
		},
	}
	if !l.isArrayType("Vector") {
		t.Fatal("named array should be recognized")
	}
	if !l.isSliceType("Scores") {
		t.Fatal("named slice should be recognized")
	}
	if got := l.sliceResultType("Vector"); got != "Slice<Counter>" {
		t.Fatalf("array slice result type = %q, want Slice<Counter>", got)
	}
	if got, ok := l.pointerArrayType("Ptr<Vector>"); !ok || got != "Array<2, Counter>" {
		t.Fatalf("pointer array type = %q ok=%v, want Array<2, Counter>", got, ok)
	}
	if got, ok := l.pointerElementType("Ptr<example/main.Counter>"); !ok || got != "Counter" {
		t.Fatalf("qualified local pointer elem = %q ok=%v, want Counter", got, ok)
	}
	if got := l.indexElementType("Scores"); got != "Counter" {
		t.Fatalf("slice index element type = %q, want Counter", got)
	}
	if got := l.indexElementType("Map<String, Counter>"); got != "Counter" {
		t.Fatalf("map index element type = %q, want Counter", got)
	}
	if got, ok := l.sliceElementType("Scores"); !ok || got != "Counter" {
		t.Fatalf("slice element type = %q ok=%v, want Counter", got, ok)
	}
	if got, ok := l.conversionArrayElementType("Vector"); !ok || got != "Counter" {
		t.Fatalf("conversion array element type = %q ok=%v, want Counter", got, ok)
	}
	key, value, ok := l.mapKeyValueTypes("Map<Counter, Scores>")
	if !ok || key != "Counter" || value != "Scores" {
		t.Fatalf("map key/value = %q/%q ok=%v, want Counter/Scores", key, value, ok)
	}
}
