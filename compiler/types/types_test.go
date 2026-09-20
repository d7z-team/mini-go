package types

import (
	"strings"
	"testing"
)

func TestTypeTableIncrementalIndexIsIsolatedAfterValueCopy(t *testing.T) {
	original := &TypeTable{}
	first := TypeNode{
		ID: "node.first", Kind: Named,
		Identity:   TypeKey{ModulePath: "example", DeclID: "First"},
		Underlying: Builtin(PrimitiveInt),
	}
	if err := original.Add(first); err != nil {
		t.Fatal(err)
	}

	copyTable := *original
	second := TypeNode{
		ID: "node.second", Kind: Named,
		Identity:   TypeKey{ModulePath: "example", DeclID: "Second"},
		Underlying: Builtin(PrimitiveString),
	}
	if err := copyTable.Add(second); err != nil {
		t.Fatal(err)
	}
	if _, ok := copyTable.Named(second.Identity); !ok {
		t.Fatal("copied table did not index appended node")
	}
	if _, ok := original.Named(second.Identity); ok {
		t.Fatal("appending to copied table modified original index")
	}
	if _, ok := original.Named(first.Identity); !ok {
		t.Fatal("original table lost existing index")
	}

	replaced := second
	replaced.Underlying = Builtin(PrimitiveBool)
	if err := copyTable.Replace(replaced); err != nil {
		t.Fatal(err)
	}
	changedIdentity := replaced
	changedIdentity.Identity.DeclID = "Other"
	if err := copyTable.Replace(changedIdentity); err == nil {
		t.Fatal("replace accepted a changed type identity")
	}
}

func TestTypeTableValidatesRecursivePointerAndArray(t *testing.T) {
	table := &TypeTable{}
	intType := Builtin(PrimitiveInt64)
	if err := table.Add(TypeNode{ID: "node.pointer", Kind: Pointer, Elem: TypeRef{Kind: Pointer, Node: "node.pointer"}}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: "node.array", Kind: Array, Length: 3, Elem: intType}); err != nil {
		t.Fatal(err)
	}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := Format(TypeRef{Kind: Array, Node: "node.array"}); got != "array" {
		t.Fatalf("format without table must stay opaque, got %q", got)
	}
}

func TestTypeTableRejectsAliasCycle(t *testing.T) {
	table := NewTable(
		TypeNode{ID: "alias.a", Kind: Named, Identity: TypeKey{ModulePath: "example", DeclID: "A"}, Alias: true, AliasTarget: TypeRef{Kind: Named, Node: "alias.b"}},
		TypeNode{ID: "alias.b", Kind: Named, Identity: TypeKey{ModulePath: "example", DeclID: "B"}, Alias: true, AliasTarget: TypeRef{Kind: Named, Node: "alias.a"}},
	)
	if err := table.Validate(); err == nil || !strings.Contains(err.Error(), "alias cycle") {
		t.Fatalf("Validate() = %v, want alias cycle", err)
	}
}

func TestTypeTableKeepsUnknownArrayLengthCompilerOnly(t *testing.T) {
	table := NewTable(TypeNode{ID: "array", Kind: Array, Length: UnknownArrayLength, Elem: Builtin(PrimitiveInt)})
	if table == nil {
		t.Fatal("create type table")
	}
	if err := table.ValidateCompiler(); err != nil {
		t.Fatalf("compiler validation: %v", err)
	}
	if err := table.Validate(); err == nil {
		t.Fatal("runtime validation accepted unknown array length")
	}
}

func TestRelationsDoNotTreatUnknownArrayAsIdentical(t *testing.T) {
	table := NewTable(
		TypeNode{ID: "unknown", Kind: Array, Length: UnknownArrayLength, Elem: Builtin(PrimitiveInt)},
		TypeNode{ID: "empty", Kind: Array, Length: 0, Elem: Builtin(PrimitiveInt)},
	)
	if table == nil {
		t.Fatal("create type table")
	}
	relations := NewRelations(table)
	if relations.Identical(TypeRef{Kind: Array, Node: "unknown"}, TypeRef{Kind: Array, Node: "empty"}).OK {
		t.Fatal("unknown array length must not be identical to a valid zero-length array")
	}
}

func TestReachableTableIncludesFunctionSignatureClosure(t *testing.T) {
	table := &TypeTable{}
	slice := TypeNode{ID: "slice", Kind: Slice, Elem: Builtin(PrimitiveInt)}
	if err := table.Add(slice); err != nil {
		t.Fatal(err)
	}
	function := TypeNode{ID: "function", Kind: Function, Signature: &FunctionSignature{
		Params: []TypeParam{{Type: TypeRef{Kind: Slice, Node: slice.ID}}}, Results: []TypeRef{Builtin(PrimitiveInt)},
	}}
	if err := table.Add(function); err != nil {
		t.Fatal(err)
	}
	reachable, err := table.ReachableTable(TypeRef{Kind: Function, Node: function.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(reachable.Nodes) != 2 {
		t.Fatalf("reachable nodes = %#v", reachable.Nodes)
	}
}

func TestTypeTablePreservesDefinedAndAliasIdentity(t *testing.T) {
	table := &TypeTable{}
	intType := Builtin(PrimitiveInt64)
	if err := table.Add(TypeNode{ID: "node.score", Kind: Named, Identity: TypeKey{ModulePath: "example", DeclID: "Score"}, Underlying: intType}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: "node.level", Kind: Named, Identity: TypeKey{ModulePath: "example", DeclID: "Level"}, Underlying: intType}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: "node.alias", Kind: Named, Identity: TypeKey{ModulePath: "example", DeclID: "Alias"}, Alias: true, AliasTarget: TypeRef{Kind: Named, Node: "node.score"}}); err != nil {
		t.Fatal(err)
	}
	score := TypeRef{Kind: Named, Node: "node.score"}
	alias := TypeRef{Kind: Named, Node: "node.alias"}
	if table.Underlying(alias) != intType || table.Underlying(score) != intType {
		t.Fatalf("unexpected underlying types: alias=%#v score=%#v", table.Underlying(alias), table.Underlying(score))
	}
	if !table.Assignable(score, intType) || !table.Assignable(intType, score) || table.Assignable(score, TypeRef{Kind: Named, Node: "node.level"}) {
		t.Fatal("defined type identity must remain distinct while preserving unnamed underlying assignment")
	}
	declared := table.DeclaredNamed("example")
	if len(declared) != 3 || declared[0].Identity.DeclID != "Alias" || declared[1].Identity.DeclID != "Level" || declared[2].Identity.DeclID != "Score" {
		t.Fatalf("declared named types = %#v", declared)
	}
	defined := table.DefinedNamed("example")
	if len(defined) != 2 || defined[0].Identity.DeclID != "Level" || defined[1].Identity.DeclID != "Score" {
		t.Fatalf("defined named types = %#v", defined)
	}
}

func TestReachableTableKeepsOnlyReferencedTypeClosure(t *testing.T) {
	table := &TypeTable{}
	parser := NewParser("example/module", table)
	slice, err := parser.Parse("Slice<Array<4, Uint8>>")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse("Map<String, Int>"); err != nil {
		t.Fatal(err)
	}
	reachable, err := table.ReachableTable(slice)
	if err != nil {
		t.Fatal(err)
	}
	if len(reachable.Nodes) != 2 {
		t.Fatalf("reachable nodes = %d, want slice and array", len(reachable.Nodes))
	}
	if got := FormatWithTable(reachable, slice); got != "Slice<Array<4, Uint8>>" {
		t.Fatalf("reachable type = %s", got)
	}
}

func TestFunctionRelationIncludesVariadicBit(t *testing.T) {
	table := &TypeTable{}
	intType := Builtin(PrimitiveInt64)
	if err := table.Add(TypeNode{ID: "node.fixed", Kind: Function, Signature: &FunctionSignature{Params: []TypeParam{{Type: intType}}, Results: []TypeRef{intType}}}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: "node.variadic", Kind: Function, Signature: &FunctionSignature{Params: []TypeParam{{Type: TypeRef{Kind: Slice, Node: "node.slice"}}}, Results: []TypeRef{intType}, Variadic: true}}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: "node.slice", Kind: Slice, Elem: intType}); err != nil {
		t.Fatal(err)
	}
	fixed := TypeRef{Kind: Function, Node: "node.fixed"}
	variadic := TypeRef{Kind: Function, Node: "node.variadic"}
	if table.Assignable(fixed, variadic) || table.Assignable(variadic, fixed) {
		t.Fatal("variadic and fixed function signatures must not be assignable")
	}
}

func TestInterfaceMethodRelationUsesStructuredSignatures(t *testing.T) {
	table := &TypeTable{}
	intType := Builtin(PrimitiveInt64)
	method := Method{Name: "Read", ModulePath: "example", Signature: FunctionSignature{Results: []TypeRef{intType}}}
	if err := table.Add(TypeNode{ID: "node.reader", Kind: Interface, Methods: []Method{method}}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: "node.value", Kind: Struct, Methods: []Method{method}}); err != nil {
		t.Fatal(err)
	}
	reader := TypeRef{Kind: Interface, Node: "node.reader"}
	value := TypeRef{Kind: Struct, Node: "node.value"}
	if !table.Assignable(value, reader) || !table.Implements(value, reader) {
		t.Fatal("structured method set should satisfy interface")
	}
}

func TestNamedInterfaceQueryUsesCompleteMethodSet(t *testing.T) {
	table := &TypeTable{}
	method := Method{Name: "Read", Signature: FunctionSignature{Results: []TypeRef{Builtin(PrimitiveInt64)}}}
	shape := TypeRef{Kind: Interface, Node: "node.embedded_interface"}
	if err := table.Add(TypeNode{ID: shape.Node, Kind: Interface}); err != nil {
		t.Fatal(err)
	}
	named := TypeRef{
		Kind:  Named,
		Named: TypeKey{ModulePath: "example", DeclID: "ReadCloser"},
		Node:  "node.read_closer",
	}
	if err := table.Add(TypeNode{
		ID: named.Node, Kind: Named, Identity: named.Named,
		Underlying: shape, Methods: []Method{method},
	}); err != nil {
		t.Fatal(err)
	}
	target := TypeRef{Kind: Interface, Node: "node.reader"}
	if err := table.Add(TypeNode{ID: target.Node, Kind: Interface, Methods: []Method{method}}); err != nil {
		t.Fatal(err)
	}
	queried, ok := table.IsInterface(named)
	if !ok || len(queried.Methods) != 1 || queried.Methods[0].Name != "Read" {
		t.Fatalf("named interface method set = %#v, ok = %v", queried.Methods, ok)
	}
	if !table.Assignable(named, target) {
		t.Fatal("named interface complete method set should satisfy target interface")
	}
}

func TestParserPreservesDottedModulePath(t *testing.T) {
	parser := NewParser("example/main", &TypeTable{})
	ref, err := parser.Parse("github.com/d7z-team/mini-go/compiler/token.Kind")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Kind != Named || ref.Named.ModulePath != "github.com/d7z-team/mini-go/compiler/token" || ref.Named.DeclID != "Kind" {
		t.Fatalf("qualified type was split incorrectly: %#v", ref)
	}
	if got := Format(ref); got != "github.com/d7z-team/mini-go/compiler/token.Kind" {
		t.Fatalf("qualified type format = %q", got)
	}
}

func TestParseCanonicalRejectsUnknownNamedType(t *testing.T) {
	parser := NewParser("example/main", &TypeTable{})
	if _, err := parser.Parse("Missing"); err != nil {
		t.Fatalf("source-oriented Parse rejected a name before semantic resolution: %v", err)
	}
	if _, err := parser.ParseCanonical("Slice<Missing>"); err == nil || !strings.Contains(err.Error(), "unknown canonical named type") {
		t.Fatalf("ParseCanonical error = %v", err)
	}

	table := NewTable(TypeNode{
		ID: "decl.example/main.Value", Kind: Named,
		Identity: TypeKey{ModulePath: "example/main", DeclID: "Value"}, Underlying: AnyType(),
	})
	if _, err := NewParser("example/main", table).ParseCanonical("Slice<Value>"); err != nil {
		t.Fatalf("ParseCanonical rejected known named type: %v", err)
	}
}

func TestBuiltinTypeNames(t *testing.T) {
	for _, name := range []string{"Void", "Any", "Int", "Function"} {
		if !IsBuiltinTypeName(name) {
			t.Fatalf("expected %q to be a builtin type name", name)
		}
	}
	for _, name := range []string{"Counter", "Slice", "function"} {
		if IsBuiltinTypeName(name) {
			t.Fatalf("did not expect %q to be a builtin type name", name)
		}
	}
}

func TestRelationsAliasCycleTerminates(t *testing.T) {
	table := &TypeTable{}
	left := TypeRef{Kind: Named, Named: TypeKey{ModulePath: "example", DeclID: "Left"}, Node: "node.left"}
	right := TypeRef{Kind: Named, Named: TypeKey{ModulePath: "example", DeclID: "Right"}, Node: "node.right"}
	if err := table.Add(TypeNode{ID: left.Node, Kind: Named, Identity: left.Named, Alias: true, AliasTarget: right}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{ID: right.Node, Kind: Named, Identity: right.Named, Alias: true, AliasTarget: left}); err != nil {
		t.Fatal(err)
	}
	if got := NewRelations(table).ResolveAlias(left); got != left {
		t.Fatalf("cyclic alias resolved to %#v, want starting reference", got)
	}
}
