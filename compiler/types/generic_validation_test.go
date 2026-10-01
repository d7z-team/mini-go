package types

import "testing"

func TestCompilerValidationKeepsGenericTypesOutOfRuntimeTables(t *testing.T) {
	table := &TypeTable{}
	param := TypeRef{Kind: TypeParameter, Node: "typeparam.T"}
	if err := table.Add(TypeNode{ID: param.Node, Kind: TypeParameter, Constraint: AnyType()}); err != nil {
		t.Fatal(err)
	}
	if err := table.Add(TypeNode{
		ID: "instance.Box.Int", Kind: Instance,
		Base:     TypeRef{Kind: Named, Named: TypeKey{ModulePath: "example/lib", DeclID: "Box"}},
		TypeArgs: []TypeRef{Builtin(PrimitiveInt)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := table.ValidateCompiler(); err != nil {
		t.Fatalf("ValidateCompiler failed: %v", err)
	}
	if err := table.Validate(); err == nil {
		t.Fatal("runtime type-table validation accepted generic nodes")
	}
}

func TestInstanceTextPreservesDeclarationAndArguments(t *testing.T) {
	table := &TypeTable{}
	instance := TypeRef{Kind: Instance, Node: "instance.Seq.String"}
	if err := table.Add(TypeNode{
		ID:       instance.Node,
		Kind:     Instance,
		Base:     TypeRef{Kind: Named, Named: TypeKey{ModulePath: "iter", DeclID: "Seq"}},
		TypeArgs: []TypeRef{Builtin(PrimitiveString)},
	}); err != nil {
		t.Fatal(err)
	}
	text := FormatWithTable(table, instance)
	if text != "Instance<iter.Seq, String>" {
		t.Fatalf("FormatWithTable(instance) = %q", text)
	}
	parsed, err := NewParser("iter", table).Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := table.Node(parsed)
	if !ok || node.Kind != Instance || node.Base.Named.DeclID != "Seq" || len(node.TypeArgs) != 1 || node.TypeArgs[0] != Builtin(PrimitiveString) {
		t.Fatalf("instance round trip: %+v", node)
	}
}

func TestFormatComparableConstraint(t *testing.T) {
	table := &TypeTable{}
	constraint := TypeRef{Kind: Interface, Node: "universe.comparable"}
	if err := table.Add(TypeNode{ID: constraint.Node, Kind: Interface, Name: "comparable", TypeSet: true}); err != nil {
		t.Fatal(err)
	}
	if got := FormatWithTable(table, constraint); got != "Comparable" {
		t.Fatalf("FormatWithTable(comparable) = %q", got)
	}
}
