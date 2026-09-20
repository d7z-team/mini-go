package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestRuntimeTypeStructuredAccessors(t *testing.T) {
	fnType := runtimeTypeFromText("function(Int, variadic Slice<String>) Bool")
	fn, ok := fnType.FunctionInfo()
	if !ok || !fn.Signature.Variadic || types.FormatSignature(fnType.Table, fn.Signature) != "function(Int, variadic Slice<String>) Bool" {
		t.Fatalf("unexpected function info: ok=%v signature=%s variadic=%v", ok, types.FormatSignature(fnType.Table, fn.Signature), fn.Signature.Variadic)
	}

	signature, variadic, ok := (*moduleInstance)(nil).functionTypeInfo("function(Int, variadic Slice<String>) Bool")
	if !ok || !variadic || signature != "function(Int, Slice<String>) Bool" {
		t.Fatalf("unexpected normalized function info: signature=%q variadic=%v ok=%v", signature, variadic, ok)
	}

	ifaceType := runtimeTypeFromText("interface{Read:function() Int}")
	iface, ok := ifaceType.InterfaceInfo()
	if !ok || len(iface.Methods) != 1 || iface.Methods[0].Name != "Read" || iface.TypeSet {
		t.Fatalf("unexpected interface info: ok=%v methods=%v typeSet=%v", ok, iface.Methods, iface.TypeSet)
	}
}

func TestRuntimePredeclaredTypesDoNotBuildTransientTables(t *testing.T) {
	for _, name := range []string{"Void", "Any", "Bool", "String", "Int", "Uint64", "Float64"} {
		runtimeType := runtimeTypeFromText(name)
		if !runtimeType.Valid() || runtimeType.Table != nil || runtimeType.String() != name {
			t.Fatalf("runtime type %q = %#v", name, runtimeType)
		}
	}
}

func TestRuntimeTypeTextCacheEvictionPreservesIdentity(t *testing.T) {
	const originalText = "struct{Value:Array<7, Int>}"
	original := runtimeTypeFromText(originalText)
	slot := runtimeTypeCacheIndex(originalText)
	collision := ""
	for length := 1; length < parsedRuntimeTypeCacheSlots*32; length++ {
		candidate := fmt.Sprintf("Array<%d, Uint8>", length)
		if candidate != originalText && runtimeTypeCacheIndex(candidate) == slot {
			collision = candidate
			break
		}
	}
	if collision == "" {
		t.Fatal("failed to find bounded runtime type cache collision")
	}
	runtimeTypeFromText(collision)
	reparsed := runtimeTypeFromText(originalText)
	if original.Table == reparsed.Table {
		t.Fatal("cache collision did not evict the original parsed table")
	}
	if !original.Equal(reparsed) {
		t.Fatalf("reparsed standalone type lost identity: %s != %s", original, reparsed)
	}

	oversized := "struct{" + strings.Repeat("X", maxCachedRuntimeTypeLength) + ":Int}"
	runtimeTypeFromText(oversized)
	parsedRuntimeTypeCache.RLock()
	defer parsedRuntimeTypeCache.RUnlock()
	for _, entry := range parsedRuntimeTypeCache.entries {
		if entry.text == oversized {
			t.Fatal("oversized runtime type text was retained by the process cache")
		}
	}
}

func TestRuntimeNamedTypesWithLocalRefsRemainModuleDistinct(t *testing.T) {
	ref := types.TypeRef{Kind: types.Named, Node: "decl.Reader"}
	leftTable := types.NewTable(types.TypeNode{
		ID: "decl.Reader", Kind: types.Named,
		Identity:   types.TypeKey{ModulePath: "example/left", DeclID: "Reader"},
		Underlying: types.Builtin(types.PrimitiveInt),
	})
	rightTable := types.NewTable(types.TypeNode{
		ID: "decl.Reader", Kind: types.Named,
		Identity:   types.TypeKey{ModulePath: "example/right", DeclID: "Reader"},
		Underlying: types.Builtin(types.PrimitiveInt),
	})
	left := runtimeTypeWithTable(ref, leftTable)
	right := runtimeTypeWithTable(ref, rightTable)
	if left.Equal(right) {
		t.Fatal("local named references from different type tables compared equal")
	}
}

func TestRuntimeStructuredComparableShapes(t *testing.T) {
	module := (*moduleInstance)(nil)
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{name: "array of int", typ: "Array<2, Int>", want: true},
		{name: "array of slice", typ: "Array<1, Slice<Int>>", want: false},
		{name: "struct of map", typ: "struct{Values:Map<String, Int>}", want: false},
		{name: "pointer", typ: "Ptr<Int>", want: true},
		{name: "function", typ: "function() Int", want: false},
	}
	for _, tt := range tests {
		got, err := module.isComparableRuntimeType(tt.typ, map[string]struct{}{})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.name, err)
		}
		if got != tt.want {
			t.Fatalf("%s: comparable=%v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNamedStringConversionProducesPredeclaredString(t *testing.T) {
	identity := types.TypeKey{ModulePath: "example/module", DeclID: "Name"}
	table := types.NewTable(types.TypeNode{
		ID:         "type.Name",
		Kind:       types.Named,
		Identity:   identity,
		Underlying: types.TypeRef{Kind: types.Primitive, Primitive: types.PrimitiveString},
	})
	if table == nil {
		t.Fatal("create named string type table")
	}
	value := newVMValue(vmType{
		Ref:   types.TypeRef{Kind: types.Named, Named: identity},
		Table: table,
	}, "mini")

	converted, err := (&moduleInstance{}).convertValue(value, "String")
	if err != nil {
		t.Fatalf("convert named string: %v", err)
	}
	if !converted.Type.Equal(coerceRuntimeType("String")) || converted.Data != "mini" {
		t.Fatalf("unexpected converted value: type=%s data=%#v", converted.Type, converted.Data)
	}
}

func TestWaitableDirectionConversionPreservesResource(t *testing.T) {
	module := &moduleInstance{}
	resource := &waitableResource{}
	value := newVMValue("Waitable<Int>", resource)

	converted, err := module.convertValue(value, "SendWaitable<Int>")
	if err != nil {
		t.Fatalf("convert bidirectional channel: %v", err)
	}
	if converted.Type.String() != "SendWaitable<Int>" || converted.Data != resource {
		t.Fatalf("converted channel = %#v", converted)
	}

	receiveOnly := newVMValue("ReceiveWaitable<Int>", resource)
	if _, err := module.convertValue(receiveOnly, "SendWaitable<Int>"); err == nil {
		t.Fatal("receive-only channel converted to send-only channel")
	}
}

func TestRuntimeTypeRebindsImportedNamedTypeToOwnerCatalog(t *testing.T) {
	identity := types.TypeKey{ModulePath: "example/dep", DeclID: "Reader"}
	interfaceType := runtimeTypeFromText("interface{Read:function() Int}")
	ownerTable := *interfaceType.Table
	if err := ownerTable.Add(types.TypeNode{
		ID:         "decl.example.dep.Reader",
		Kind:       types.Named,
		Identity:   identity,
		Underlying: interfaceType.Ref,
	}); err != nil {
		t.Fatalf("add named interface type: %v", err)
	}
	readerType, _ := ownerTable.Named(identity)
	registry := newModuleRegistry()
	owner := &executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "example/dep"}, TypeTable: ownerTable},
		Types: map[string]types.TypeNode{
			"Reader": readerType,
		},
	}
	consumer := &executable{Artifact: ir.Artifact{Module: ir.Module{Path: "example/main"}}, Types: map[string]types.TypeNode{}}
	if err := registry.addExecutable(owner); err != nil {
		t.Fatalf("add owner module: %v", err)
	}
	if err := registry.addExecutable(consumer); err != nil {
		t.Fatalf("add consumer module: %v", err)
	}
	consumerModule, _ := registry.module("example/main")
	consumerTable := &types.TypeTable{}
	placeholder, err := types.NewParser("example/main", consumerTable).Parse("example/dep.Reader")
	if err != nil {
		t.Fatalf("parse imported named type: %v", err)
	}
	resolved := consumerModule.resolvedRuntimeType(vmType{Ref: placeholder, Table: consumerTable})
	if resolved.Table != &owner.Artifact.TypeTable || resolved.ShapeKind() != types.Interface {
		t.Fatalf("imported named type was not rebound to owner catalog: type=%s shape=%d", resolved, resolved.ShapeKind())
	}
}

func TestRuntimeTypeResolvesCrossPackageAlias(t *testing.T) {
	fileModeKey := types.TypeKey{ModulePath: "io/fs", DeclID: "FileMode"}
	fileMode := types.TypeNode{
		ID: "decl.io/fs.FileMode", Kind: types.Named, Identity: fileModeKey,
		Underlying: types.Builtin(types.PrimitiveUint32),
	}
	fsTable := types.NewTable(fileMode)
	aliasKey := types.TypeKey{ModulePath: "os", DeclID: "FileMode"}
	alias := types.TypeNode{
		ID: "decl.os.FileMode", Kind: types.Named, Identity: aliasKey, Alias: true,
		AliasTarget: types.TypeRef{Kind: types.Named, Named: fileModeKey},
	}
	osTable := types.NewTable(alias)
	registry := newModuleRegistry()
	if err := registry.addExecutable(&executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "io/fs"}, TypeTable: *fsTable},
		Types:    map[string]types.TypeNode{"FileMode": fileMode},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.addExecutable(&executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "os"}, TypeTable: *osTable},
		Types:    map[string]types.TypeNode{"FileMode": alias},
	}); err != nil {
		t.Fatal(err)
	}
	osModule, _ := registry.module("os")
	value := newVMValue(vmType{Ref: types.TypeRef{Kind: types.Named, Named: fileModeKey}, Table: fsTable}, uint64(0o644))
	resolved, err := osModule.coerceAssignableValue(value, "os.FileMode")
	if err != nil {
		t.Fatalf("assign alias value: %v", err)
	}
	if resolved.Type.String() != "io/fs.FileMode" {
		t.Fatalf("resolved alias type = %s", resolved.Type)
	}
	parser := types.NewParser("os", osTable)
	aliasPointer, err := parser.Parse("Ptr<os.FileMode>")
	if err != nil {
		t.Fatal(err)
	}
	originalPointer, err := parser.Parse("Ptr<io/fs.FileMode>")
	if err != nil {
		t.Fatal(err)
	}
	left := newVMValue(runtimeTypeWithTable(aliasPointer, osTable), nil)
	right := newVMValue(runtimeTypeWithTable(originalPointer, osTable), nil)
	if equal, err := osModule.equalValues(left, right); err != nil || !equal {
		t.Fatalf("alias pointer equality = %v, %v", equal, err)
	}
}

func TestPointerConversionPreservesStorageAndNamedIdentity(t *testing.T) {
	left := types.TypeNode{ID: "left", Kind: types.Named, Identity: types.TypeKey{ModulePath: "example", DeclID: "Left"}, Underlying: types.Builtin(types.PrimitiveInt)}
	right := types.TypeNode{ID: "right", Kind: types.Named, Identity: types.TypeKey{ModulePath: "example", DeclID: "Right"}, Underlying: types.Builtin(types.PrimitiveInt)}
	table := types.NewTable(left, right)
	registry := newModuleRegistry()
	if err := registry.addExecutable(&executable{Artifact: ir.Artifact{Module: ir.Module{Path: "example"}, TypeTable: *table}, Types: map[string]types.TypeNode{"Left": left, "Right": right}}); err != nil {
		t.Fatal(err)
	}
	module, _ := registry.module("example")
	stored := newVMValue("example.Left", int64(1))
	original := reflectCellPointer(module, "example.Left", "stored", stored)
	converted, err := module.convertValue(original, "Ptr<example.Right>")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := derefPointer(converted)
	if err != nil || loaded.Type.String() != "example.Right" {
		t.Fatalf("converted load: %v, %v", loaded.Type, err)
	}
	if err := storePointer(converted, newVMValue("example.Right", int64(9))); err != nil {
		t.Fatal(err)
	}
	stored = original.Data.(*vmPointer).cell.load()
	if stored.Type.String() != "example.Left" || stored.materializedData() != int64(9) {
		t.Fatalf("converted store: %#v", stored)
	}
	back, err := module.convertValue(converted, "Ptr<example.Left>")
	if err != nil {
		t.Fatal(err)
	}
	if back.Data.(*vmPointer).Identity != original.Data.(*vmPointer).Identity {
		t.Fatal("conversion changed pointer identity")
	}
	if _, err := module.convertValue(original, "Ptr<String>"); err == nil {
		t.Fatal("converted incompatible pointee")
	}
	if _, err := module.coerceAssignableValue(original, "Ptr<example.Right>"); err == nil {
		t.Fatal("distinct pointer types became assignable")
	}
	nilPointer, err := module.convertValue(newVMValue("Ptr<example.Left>", (*vmPointer)(nil)), "Ptr<example.Right>")
	if err != nil || nilPointer.Data.(*vmPointer) != nil {
		t.Fatalf("nil conversion: %#v %v", nilPointer, err)
	}
}

func TestRuntimeTypeLocalizeAndQualifyUseStructuredRewrite(t *testing.T) {
	module := &moduleInstance{executable: &executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "example/main"}},
		Types: map[string]types.TypeNode{
			"Function": {},
			"Reader":   {},
			"User":     {},
		},
	}}

	qualified := module.qualifyLocalType("Map<String, function(Ptr<User>, variadic Slice<User>) struct{Owner:User `json:\"owner\"`,Peer:example/lib.User}>")
	wantQualified := "Map<String, function(Ptr<example/main.User>, variadic Slice<example/main.User>) struct{Owner:example/main.User `json:\"owner\"`,Peer:example/lib.User}>"
	if qualified != wantQualified {
		t.Fatalf("qualified type mismatch:\n got: %s\nwant: %s", qualified, wantQualified)
	}

	localized := module.localizeType("interface{Read:function(example/main.User) Void,example/lib.Reader,~example/main.User|String}")
	wantLocalized := "interface{~User|String,example/lib.Reader,Read:function(User) Void}"
	if localized != wantLocalized {
		t.Fatalf("localized type mismatch:\n got: %s\nwant: %s", localized, wantLocalized)
	}

	unknown := module.qualifyLocalType("Slice<Unknown>")
	if unknown != "Slice<Unknown>" {
		t.Fatalf("unknown type should stay unqualified, got %s", unknown)
	}

	shadowedPrimitive := module.qualifyLocalType("Ptr<Slice<Function>>")
	if shadowedPrimitive != "Ptr<Slice<example/main.Function>>" {
		t.Fatalf("local type shadowing primitive was not qualified, got %s", shadowedPrimitive)
	}
}

func TestQualifiedTypeCacheInvalidatesWhenRegistryChanges(t *testing.T) {
	registry := newModuleRegistry()
	mainExecutable := &executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "example/main"}},
		Types:    map[string]types.TypeNode{},
	}
	if err := registry.addExecutable(mainExecutable); err != nil {
		t.Fatalf("add main executable: %v", err)
	}
	mainModule, _ := registry.module("example/main")
	if _, _, ok := mainModule.qualifiedTypeModule("example/dep.Item"); ok {
		t.Fatal("type resolved before dependency was loaded")
	}
	if _, ok := mainModule.interfaceType("example/dep.Item"); ok {
		t.Fatal("interface type resolved before dependency was loaded")
	}

	interfaceType := runtimeTypeFromText("interface{}")
	itemIdentity := types.TypeKey{ModulePath: "example/dep", DeclID: "Item"}
	itemType := types.TypeNode{ID: "decl.example.dep.Item", Kind: types.Named, Identity: itemIdentity, Underlying: interfaceType.Ref}
	if err := interfaceType.Table.Add(itemType); err != nil {
		t.Fatalf("add dependency type: %v", err)
	}
	dependency := &executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "example/dep"}, TypeTable: *interfaceType.Table},
		Types: map[string]types.TypeNode{
			"Item": itemType,
		},
	}
	if err := registry.addExecutable(dependency); err != nil {
		t.Fatalf("add dependency executable: %v", err)
	}
	resolvedModule, name, ok := mainModule.qualifiedTypeModule("example/dep.Item")
	if !ok || resolvedModule == nil || resolvedModule.executable != dependency || name != "Item" {
		t.Fatalf("type did not resolve after registry update: module=%p name=%q ok=%v", resolvedModule, name, ok)
	}
	if resolved, ok := mainModule.interfaceType("example/dep.Item"); !ok || resolved != "example/dep.Item" {
		t.Fatalf("interface type did not resolve after registry update: type=%q ok=%v", resolved, ok)
	}
}

func TestRuntimeFunctionSignaturePartsUseStructuredParser(t *testing.T) {
	params, result, ok := parseFunctionSignatureParts("function(User, variadic Slice<User>) tuple(String, User)")
	if !ok {
		t.Fatal("function signature was not parsed")
	}
	if len(params) != 2 || params[0] != "User" || params[1] != "variadic Slice<User>" || result != "tuple(String, User)" {
		t.Fatalf("unexpected signature parts: params=%v result=%q", params, result)
	}

	params, result, ok = parseFunctionSignatureParts("function()")
	if !ok || len(params) != 0 || result != "Void" {
		t.Fatalf("unexpected void signature parts: params=%v result=%q ok=%v", params, result, ok)
	}

	elem, ok := runtimeSliceElemText("Slice<User>")
	if !ok || elem != "User" {
		t.Fatalf("unexpected slice elem: %q ok=%v", elem, ok)
	}
}

func TestRuntimeTypeTreatsDefinedAnyAsEmptyInterface(t *testing.T) {
	table := types.NewTable(types.TypeNode{
		ID:         "token",
		Kind:       types.Named,
		Identity:   types.TypeKey{ModulePath: "example", DeclID: "Token"},
		Underlying: types.AnyType(),
	})
	token := vmType{Ref: types.TypeRef{Kind: types.Named, Node: "token"}, Table: table}
	if token.ShapeKind() != types.Interface {
		t.Fatalf("defined any shape = %v, want interface", token.ShapeKind())
	}
	if info, ok := token.InterfaceInfo(); !ok || len(info.Methods) != 0 || info.TypeSet {
		t.Fatalf("defined any interface info = %+v, %v", info, ok)
	}
}
