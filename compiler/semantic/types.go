package semantic

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/token"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (a *analyzer) analyzeType(typ *ast.TypeExpr, scope ScopeID) {
	if typ == nil || typ.Kind == ast.TypeInvalid {
		return
	}
	a.info.NodeScopes[typ.NodeID] = scope
	ref := a.typeOf(*typ)
	if typ.Kind == ast.TypeName {
		name := typ.Name
		if object, ok := a.info.Lookup(scope, name); ok && (object.Kind == ObjectType || object.Kind == ObjectTypeParam) {
			a.info.Uses[typ.NodeID] = object.ID
			ref = object.Type
		} else if dot := strings.IndexByte(name, '.'); dot > 0 {
			if object, ok := a.info.Lookup(scope, name[:dot]); ok && object.Kind == ObjectImport && object.ImportPath != "" {
				a.info.Uses[typ.NodeID] = object.ID
				ref = types.TypeRef{}
				if member, found := a.importedMember(object.ImportPath, name[:dot], name[dot+1:], typ.Span); found {
					if member.Kind != ObjectType {
						a.addDiagnostic("semantic.import.member.not_type", name+" is not a type", typ.Span)
					} else {
						ref = a.dependencyType(member)
					}
				}
			}
		} else if object, ok := a.info.Lookup(scope, name); !ok || (object.Kind != ObjectType && object.Kind != ObjectTypeParam) {
			if ref.Kind == types.Named {
				if ok {
					a.addDiagnostic("semantic.type.not_type", name+" is not a type", typ.Span)
				} else {
					a.addDiagnostic("semantic.type.unknown", "unknown type "+name, typ.Span)
				}
			}
			ref = types.TypeRef{}
		}
	}
	a.analyzeType(typ.Base, scope)
	for i := range typ.TypeArgs {
		a.analyzeType(&typ.TypeArgs[i], scope)
	}
	if typ.Kind == ast.TypeInstance && typ.Base != nil {
		base := a.resolvedType(*typ.Base)
		args := make([]types.TypeRef, len(typ.TypeArgs))
		valid := base.Valid()
		for i := range typ.TypeArgs {
			args[i] = a.resolvedType(typ.TypeArgs[i])
			valid = valid && args[i].Valid()
		}
		if valid {
			id := types.TypeID("instance." + strconv.FormatUint(uint64(typ.NodeID), 10))
			_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.Instance, Base: base, TypeArgs: args})
			ref = types.TypeRef{Kind: types.Instance, Node: id}
			a.info.Types[typ.NodeID] = TypeInfo{Type: ref}
			if baseObject := a.info.Uses[typ.Base.NodeID]; baseObject != "" {
				a.info.Instances[typ.NodeID] = InstanceInfo{Generic: baseObject, TypeArgs: args}
			}
		}
	}
	a.analyzeType(typ.Elem, scope)
	a.analyzeType(typ.Key, scope)
	if typ.Len != nil {
		a.analyzeExpr(typ.Len, scope)
	}
	for i := range typ.Params {
		a.analyzeField(&typ.Params[i], scope, false)
	}
	for i := range typ.Results {
		a.analyzeField(&typ.Results[i], scope, false)
	}
	for i := range typ.Fields {
		a.analyzeField(&typ.Fields[i], scope, false)
	}
	for i := range typ.Methods {
		a.analyzeFunc(&typ.Methods[i], scope, "")
	}
	for i := range typ.Embeds {
		a.analyzeType(&typ.Embeds[i], scope)
	}
	for i := range typ.Terms {
		a.analyzeType(&typ.Terms[i].Type, scope)
	}
	if typ.Kind != ast.TypeName && typ.Kind != ast.TypeInstance {
		ref = a.compositeType(*typ)
	}
	a.info.Types[typ.NodeID] = TypeInfo{Type: ref}
}

func (a *analyzer) compositeType(typ ast.TypeExpr) types.TypeRef {
	id := types.TypeID("source." + strconv.FormatUint(uint64(typ.NodeID), 10))
	if typ.NodeID == 0 {
		return types.TypeRef{}
	}
	node := types.TypeNode{ID: id}
	switch typ.Kind {
	case ast.TypeSlice:
		node.Kind, node.Elem = types.Slice, a.resolvedTypePtr(typ.Elem)
	case ast.TypePointer:
		node.Kind, node.Elem = types.Pointer, a.resolvedTypePtr(typ.Elem)
	case ast.TypeArray:
		node.Kind, node.Elem = types.Array, a.resolvedTypePtr(typ.Elem)
		node.Length = types.UnknownArrayLength
		if typ.Len != nil {
			if length, ok := a.arrayLength(*typ.Len, a.info.NodeScopes[typ.Len.NodeID]); ok {
				node.Length = length
			}
		}
	case ast.TypeMap:
		node.Kind, node.Key, node.Elem = types.Map, a.resolvedTypePtr(typ.Key), a.resolvedTypePtr(typ.Elem)
	case ast.TypeChan:
		node.Kind, node.Elem, node.Direction = types.Waitable, a.resolvedTypePtr(typ.Elem), types.ChannelBoth
		switch typ.Direction {
		case "recv":
			node.Direction = types.ChannelReceive
		case "send":
			node.Direction = types.ChannelSend
		}
	case ast.TypeFunc:
		node.Kind = types.Function
		signature := a.functionTypeSignature(typ.Params, typ.Results)
		node.Signature = &signature
	case ast.TypeStruct:
		node.Kind = types.Struct
		fieldNames := make(map[string]struct{}, len(typ.Fields))
		for _, field := range typ.Fields {
			name := strings.TrimSpace(field.Name)
			embedded := name == ""
			if embedded {
				name = field.EmbeddedName
				if name == "" {
					name = embeddedFieldName(field.Type)
				}
				if !a.validEmbeddedFieldType(field.Type, true) {
					a.addDiagnostic("semantic.struct.embed.invalid", "embedded field must be a named type or pointer to a named non-pointer type", field.Span)
				}
			}
			if name != "_" {
				if name == "" {
					a.addDiagnostic("semantic.struct.field.name", "struct field has no name", field.Span)
				} else if _, duplicate := fieldNames[name]; duplicate {
					a.addDiagnostic("semantic.struct.field.duplicate", "struct field "+name+" is redeclared", field.Span)
				} else {
					fieldNames[name] = struct{}{}
				}
			}
			node.Fields = append(node.Fields, types.Field{Name: name, Type: a.resolvedType(field.Type), Tag: field.Tag, Embedded: embedded})
		}
	case ast.TypeInterface:
		node.Kind = types.Interface
		for _, method := range typ.Methods {
			modulePath := ""
			if !isExported(method.Name) {
				modulePath = a.info.ModulePath
			}
			node.Methods = append(node.Methods, types.Method{Name: method.Name, ModulePath: modulePath, Signature: a.functionTypeSignature(method.Params, method.Results)})
		}
		for _, term := range typ.Terms {
			node.Terms = append(node.Terms, types.TypeTerm{Type: a.resolvedType(term.Type), Approx: term.Approx, Union: len(typ.Terms) > 1})
		}
		node.TypeSet = len(node.Terms) != 0
		for _, embedded := range typ.Embeds {
			ref := a.resolvedType(embedded)
			node.Terms = append(node.Terms, types.TypeTerm{Type: ref})
			if embeddedNode, ok := a.info.TypeTable.IsInterface(ref); ok {
				node.Methods = append(node.Methods, embeddedNode.Methods...)
				node.TypeSet = node.TypeSet || embeddedNode.TypeSet
			} else if ref.Valid() && ref.Kind != types.Any {
				node.TypeSet = true
			}
		}
	default:
		return types.TypeRef{}
	}
	if existing, ok := a.info.TypeTable.Node(types.TypeRef{Kind: node.Kind, Node: id}); ok {
		if existing.Kind == node.Kind {
			_ = a.info.TypeTable.Replace(node)
			return types.TypeRef{Kind: node.Kind, Node: id}
		}
	}
	if err := a.info.TypeTable.Add(node); err != nil {
		return types.TypeRef{}
	}
	return types.TypeRef{Kind: node.Kind, Node: id}
}

func (a *analyzer) validEmbeddedFieldType(typ ast.TypeExpr, allowUnresolved bool) bool {
	explicitPointer := typ.Kind == ast.TypePointer
	if explicitPointer {
		if typ.Elem == nil || typ.Elem.Kind == ast.TypePointer {
			return false
		}
		typ = *typ.Elem
	}
	if typ.Kind != ast.TypeName && typ.Kind != ast.TypeInstance {
		return false
	}
	ref := a.resolvedType(typ)
	if !ref.Valid() {
		return false
	}
	if allowUnresolved && ref.Kind == types.Named {
		if node, ok := a.info.TypeTable.Node(ref); ok && !node.Alias && node.Underlying.Kind == types.Any {
			return true
		}
	}
	shape := a.info.Relations.View(ref).Shape()
	if explicitPointer {
		return shape != types.Pointer && shape != types.Interface
	}
	return shape != types.Pointer
}

func embeddedFieldName(typ ast.TypeExpr) string {
	for typ.Kind == ast.TypePointer && typ.Elem != nil {
		typ = *typ.Elem
	}
	if typ.Kind == ast.TypeInstance && typ.Base != nil {
		typ = *typ.Base
	}
	name := strings.TrimSpace(typ.Name)
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = strings.TrimSpace(name[dot+1:])
	}
	return name
}

func (a *analyzer) resolvedTypePtr(typ *ast.TypeExpr) types.TypeRef {
	if typ == nil {
		return types.TypeRef{}
	}
	return a.resolvedType(*typ)
}

func (a *analyzer) functionTypeSignature(params, results []ast.Field) types.FunctionSignature {
	signature := types.FunctionSignature{}
	for _, param := range params {
		paramType := a.resolvedType(param.Type)
		if param.Variadic && paramType.Valid() && a.info.Relations.View(paramType).Shape() != types.Slice {
			id := types.TypeID("variadic." + strconv.FormatUint(uint64(param.NodeID), 10))
			node := types.TypeNode{ID: id, Kind: types.Slice, Elem: paramType}
			ref := types.TypeRef{Kind: types.Slice, Node: id}
			if _, ok := a.info.TypeTable.Node(ref); ok {
				_ = a.info.TypeTable.Replace(node)
			} else {
				_ = a.info.TypeTable.Add(node)
			}
			paramType = ref
		}
		signature.Params = append(signature.Params, types.TypeParam{Type: paramType})
		signature.Variadic = signature.Variadic || param.Variadic
	}
	for _, result := range results {
		signature.Results = append(signature.Results, a.resolvedType(result.Type))
	}
	return signature
}

func (a *analyzer) typeOf(typ ast.TypeExpr) types.TypeRef {
	if typ.Kind != ast.TypeName {
		return types.TypeRef{}
	}
	text := canonicalSourceTypeName(typ.Name)
	ref, err := a.parser.Parse(text)
	if err != nil {
		return types.TypeRef{}
	}
	return ref
}

func canonicalSourceTypeName(name string) string {
	switch strings.TrimSpace(name) {
	case "int":
		return "Int"
	case "int8":
		return "Int8"
	case "int16":
		return "Int16"
	case "int32", "rune":
		return "Int32"
	case "int64":
		return "Int64"
	case "uint":
		return "Uint"
	case "uint8", "byte":
		return "Uint8"
	case "uint16":
		return "Uint16"
	case "uint32":
		return "Uint32"
	case "uint64":
		return "Uint64"
	case "uintptr":
		return "Uintptr"
	case "float32":
		return "Float32"
	case "float64":
		return "Float64"
	case "complex64":
		return "Complex64"
	case "complex128":
		return "Complex128"
	case "string":
		return "String"
	case "bool":
		return "Bool"
	case "any":
		return "Any"
	case "error":
		return "interface{Error:function() String}"
	default:
		return strings.TrimSpace(name)
	}
}

func (a *analyzer) resolvedType(typ ast.TypeExpr) types.TypeRef {
	if info, ok := a.info.Types[typ.NodeID]; ok && info.Type.Valid() {
		return info.Type
	}
	return a.typeOf(typ)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isExported(name string) bool {
	return token.IsExportedName(name)
}
