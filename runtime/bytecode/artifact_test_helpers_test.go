package bytecode

import (
	"strings"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
)

var (
	testTypeNodeMu sync.Mutex
	testTypeNodes  = map[types.TypeID]types.TypeNode{}
)

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
	parser := types.NewParser("example/module", &types.TypeTable{})
	ref, err := parser.Parse(text)
	if err != nil {
		panic(err)
	}
	testTypeNodeMu.Lock()
	for _, node := range parser.Table.Nodes {
		testTypeNodes[node.ID] = node
	}
	testTypeNodeMu.Unlock()
	return ref
}

func testSignature(text string) types.FunctionSignature {
	parser := types.NewParser("example/module", &types.TypeTable{})
	ref, err := parser.Parse(text)
	if err != nil {
		panic(err)
	}
	node, ok := parser.Table.Node(ref)
	if !ok || node.Signature == nil {
		panic("invalid function signature: " + text)
	}
	testTypeNodeMu.Lock()
	for _, node := range parser.Table.Nodes {
		testTypeNodes[node.ID] = node
	}
	testTypeNodeMu.Unlock()
	return *node.Signature
}

func testValidateArtifact(artifact *Artifact) error {
	if artifact == nil {
		return ValidateArtifact(nil)
	}
	testTypeNodeMu.Lock()
	defer testTypeNodeMu.Unlock()
	includeInvalid := false
	for _, node := range artifact.TypeTable.Nodes {
		if strings.HasSuffix(string(node.ID), ".invalid") || strings.HasSuffix(string(node.Underlying.Node), ".invalid") {
			includeInvalid = true
			break
		}
	}
	for _, node := range testTypeNodes {
		if strings.HasSuffix(string(node.ID), ".invalid") && !includeInvalid {
			continue
		}
		if _, ok := artifact.TypeTable.Node(types.TypeRef{Kind: node.Kind, Node: node.ID}); !ok {
			if err := artifact.TypeTable.Add(node); err != nil {
				return err
			}
		}
	}
	return ValidateArtifact(artifact)
}

func setTestNamedTypes(artifact *Artifact, namedTypes []testNamedType) {
	for _, namedType := range namedTypes {
		underlying := namedType.Underlying
		if !underlying.Valid() {
			underlying = namedType.Type
		}
		identity := types.TypeKey{ModulePath: artifact.Module.Path, DeclID: types.DeclID(namedType.Name)}
		if len(namedType.Fields) != 0 {
			testTypeNodeMu.Lock()
			shape, ok := testTypeNodes[underlying.Node]
			testTypeNodeMu.Unlock()
			if ok && shape.Kind == types.Struct {
				shape.Fields = make([]types.Field, 0, len(namedType.Fields))
				for _, field := range namedType.Fields {
					shape.Fields = append(shape.Fields, types.Field{Name: field.Name, Type: field.Type, Tag: field.Tag, Embedded: field.Embedded})
				}
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

func attachTestTypeNodes(artifact *Artifact) {
	testTypeNodeMu.Lock()
	defer testTypeNodeMu.Unlock()
	for _, node := range testTypeNodes {
		if strings.HasSuffix(string(node.ID), ".invalid") {
			continue
		}
		if _, ok := artifact.TypeTable.Node(types.TypeRef{Kind: node.Kind, Node: node.ID}); !ok {
			if err := artifact.TypeTable.Add(node); err != nil {
				panic(err)
			}
		}
	}
}

func testInvalidNestedVariadicType() types.TypeRef {
	ref := testType("Slice<function(Slice<Int64>, Int64) Void>")
	testTypeNodeMu.Lock()
	defer testTypeNodeMu.Unlock()
	slice := testTypeNodes[ref.Node]
	function := testTypeNodes[slice.Elem.Node]
	if function.Signature != nil {
		signature := *function.Signature
		signature.Params = append([]types.TypeParam(nil), signature.Params...)
		signature.Results = append([]types.TypeRef(nil), signature.Results...)
		function.Signature = &signature
	}
	function.Signature.Variadic = true
	function.ID += ".invalid"
	slice.ID += ".invalid"
	slice.Elem.Node = function.ID
	testTypeNodes[function.ID] = function
	testTypeNodes[slice.ID] = slice
	return types.TypeRef{Kind: types.Slice, Node: slice.ID}
}
