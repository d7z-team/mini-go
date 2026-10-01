package runtime

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

var testTypeTable = &types.TypeTable{}

type testNamedType struct {
	Name       string
	Type       types.TypeRef
	Underlying types.TypeRef
	Alias      bool
	Fields     []testTypeField
	Methods    []testTypeMethod
}

type testTypeField struct {
	Name     string
	Type     types.TypeRef
	Tag      string
	Embedded bool
}

type testTypeMethod struct {
	Name       string
	Receiver   types.TypeRef
	Signature  types.FunctionSignature
	FunctionID string
	ModulePath string
}

func testType(text string) types.TypeRef {
	testingType := types.NewParser("testing", testTypeTable)
	ref, err := testingType.Parse(text)
	if err != nil {
		panic(err)
	}
	return ref
}

func testSignature(text string) types.FunctionSignature {
	testingType := types.NewParser("testing", testTypeTable)
	ref, err := testingType.Parse(text)
	if err != nil {
		panic(err)
	}
	node, ok := testingType.Table.Node(ref)
	if !ok || node.Signature == nil {
		panic("test signature is not a function type")
	}
	return *node.Signature
}

func attachRuntimeTestTypeNodes(artifact *ir.Artifact) {
	if artifact == nil {
		return
	}
	for i := range artifact.Constants {
		rebindTestTypeRef(&artifact.Constants[i].Type, artifact.Module.Path)
	}
	for i := range artifact.Globals {
		rebindTestTypeRef(&artifact.Globals[i].Type, artifact.Module.Path)
	}
	for i := range artifact.Functions {
		rebindTestSignature(&artifact.Functions[i].Signature, artifact.Module.Path)
		if code := artifact.Functions[i].Code; code != nil {
			for j := range code.Types {
				rebindTestTypeRef(&code.Types[j], artifact.Module.Path)
			}
		}
		for j := range artifact.Functions[i].Locals {
			rebindTestTypeRef(&artifact.Functions[i].Locals[j].Type, artifact.Module.Path)
		}
		for j := range artifact.Functions[i].Upvalues {
			rebindTestTypeRef(&artifact.Functions[i].Upvalues[j].Type, artifact.Module.Path)
		}
	}
	for i := range artifact.Exports {
		rebindTestTypeRef(&artifact.Exports[i].Type, artifact.Module.Path)
	}
	for _, node := range testTypeTable.Nodes {
		node = rebindTestTypeNode(node, artifact.Module.Path)
		if _, ok := artifact.TypeTable.Node(types.TypeRef{Kind: node.Kind, Node: node.ID}); ok {
			continue
		}
		if err := artifact.TypeTable.Add(node); err != nil {
			panic(err)
		}
	}
}

func setRuntimeTestNamedTypes(artifact *ir.Artifact, namedTypes []testNamedType) {
	for _, namedType := range namedTypes {
		underlying := namedType.Underlying
		if !underlying.Valid() {
			underlying = namedType.Type
		}
		rebindTestTypeRef(&underlying, artifact.Module.Path)
		if len(namedType.Fields) != 0 {
			shape, ok := testTypeTable.Node(namedType.Underlying)
			if !ok {
				shape, ok = testTypeTable.Node(namedType.Type)
			}
			if ok && shape.Kind == types.Struct {
				shape.Fields = make([]types.Field, 0, len(namedType.Fields))
				for _, field := range namedType.Fields {
					rebindTestTypeRef(&field.Type, artifact.Module.Path)
					shape.Fields = append(shape.Fields, types.Field{Name: field.Name, Type: field.Type, Tag: field.Tag, Embedded: field.Embedded})
				}
				shape = rebindTestTypeNode(shape, artifact.Module.Path)
				if existing, found := artifact.TypeTable.Node(types.TypeRef{Kind: shape.Kind, Node: shape.ID}); found {
					shape.ID = existing.ID
					if err := artifact.TypeTable.Replace(shape); err != nil {
						panic(err)
					}
				} else if err := artifact.TypeTable.Add(shape); err != nil {
					panic(err)
				}
			}
		}
		identity := types.TypeKey{ModulePath: artifact.Module.Path, DeclID: types.DeclID(namedType.Name)}
		node := types.TypeNode{
			ID:         types.TypeID("decl." + artifact.Module.Path + "." + namedType.Name),
			Kind:       types.Named,
			Identity:   identity,
			Underlying: underlying,
			Alias:      namedType.Alias,
		}
		if namedType.Alias {
			node.AliasTarget = underlying
			node.Underlying = types.TypeRef{}
		}
		for _, method := range namedType.Methods {
			rebindTestTypeRef(&method.Receiver, artifact.Module.Path)
			rebindTestSignature(&method.Signature, artifact.Module.Path)
			node.Methods = append(node.Methods, types.Method{
				Name: method.Name, Receiver: method.Receiver, Signature: method.Signature,
				FunctionID: method.FunctionID, ModulePath: method.ModulePath,
			})
		}
		ref := types.TypeRef{Kind: types.Named, Named: identity, Node: node.ID}
		if _, exists := artifact.TypeTable.Node(ref); exists {
			if err := artifact.TypeTable.Replace(node); err != nil {
				panic(err)
			}
		} else if err := artifact.TypeTable.Add(node); err != nil {
			panic(err)
		}
	}
}

func rebindTestTypeNode(node types.TypeNode, modulePath string) types.TypeNode {
	rebindTestTypeRef(&node.AliasTarget, modulePath)
	rebindTestTypeRef(&node.Underlying, modulePath)
	rebindTestTypeRef(&node.Elem, modulePath)
	rebindTestTypeRef(&node.Key, modulePath)
	for i := range node.Tuple {
		rebindTestTypeRef(&node.Tuple[i], modulePath)
	}
	for i := range node.Fields {
		rebindTestTypeRef(&node.Fields[i].Type, modulePath)
	}
	for i := range node.Methods {
		rebindTestTypeRef(&node.Methods[i].Receiver, modulePath)
		rebindTestSignature(&node.Methods[i].Signature, modulePath)
	}
	for i := range node.Terms {
		rebindTestTypeRef(&node.Terms[i].Type, modulePath)
	}
	if node.Signature != nil {
		rebindTestSignature(node.Signature, modulePath)
	}
	return node
}

func rebindTestSignature(signature *types.FunctionSignature, modulePath string) {
	if signature == nil {
		return
	}
	for i := range signature.Params {
		rebindTestTypeRef(&signature.Params[i].Type, modulePath)
	}
	for i := range signature.Results {
		rebindTestTypeRef(&signature.Results[i], modulePath)
	}
}

func rebindTestTypeRef(ref *types.TypeRef, modulePath string) {
	if ref == nil || ref.Kind != types.Named || ref.Named.ModulePath != "testing" {
		return
	}
	ref.Named.ModulePath = modulePath
}

func loadTestEngine(artifact ir.Artifact) (*vm, error) {
	return loadTestEngineWithOptions(artifact, InstanceOptions{})
}

func loadTestEngineWithOptions(artifact ir.Artifact, options InstanceOptions) (*vm, error) {
	attachRuntimeTestTypeNodes(&artifact)
	executable, err := newLoader().load(artifact)
	if err != nil {
		return nil, err
	}
	machine, err := newVMWithOptions(executable, options)
	if err == nil {
		machine.revision.Load().symbols = testSymbolIndex(&artifact)
	}
	return machine, err
}

func loadTestEngineJSON(data []byte) (*vm, error) {
	executable, err := newLoader().loadJSON(data)
	if err != nil {
		return nil, err
	}
	return newVMWithOptions(executable, InstanceOptions{})
}

func testValueMatches(got, want vmValue) bool {
	return got.Type.Equal(want.Type) && reflect.DeepEqual(got.Data, want.Data)
}

func requireValues(t *testing.T, got []vmValue, want ...vmValue) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("expected %d values, got %d: %#v", len(want), len(got), got)
	}
	for i := range got {
		if !testValueMatches(got[i], want[i]) {
			t.Fatalf("value %d: expected %#v, got %#v", i, want[i], got[i])
		}
	}
}

func testTypePayload(text string) ir.Payload {
	return ir.TypePayload{Type: exampleModuleType(text)}
}

func testArrayPayload(text string, count int) ir.Payload {
	return ir.MakeSequencePayload{Type: exampleModuleType(text), ElementCount: count}
}

func testMakeSlicePayload(text string) ir.Payload {
	return ir.MakeSlicePayload{Type: exampleModuleType(text)}
}

func testMapPayload(text string, count int) ir.Payload {
	return ir.MakeMapPayload{Type: exampleModuleType(text), EntryCount: count}
}

func testStructPayload(text string, fields ...string) ir.Payload {
	return ir.MakeStructPayload{Type: exampleModuleType(text), Fields: fields}
}

func exampleModuleType(text string) types.TypeRef {
	parser := types.NewParser("example/module", testTypeTable)
	ref, err := parser.Parse(text)
	if err != nil {
		panic(err)
	}
	return ref
}
