package semantic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

// DependencyExport is the compiler-only source semantics view of an imported
// package export. Canonical text is restricted to this artifact boundary.
type DependencyExport struct {
	ModulePath string
	Name       string
	ID         string
	Kind       ObjectKind
	Type       string
	Underlying string
	Value      json.RawMessage
	Fields     []DependencyTypeField
	Methods    []DependencyTypeMethod
	Variadic   bool
	Untyped    bool
	TypeParams []string

	// Decoded facts belong to one analyzer's dependency snapshot.
	constantValue *constant.Value
}

type DependencyTypeField struct {
	Name     string
	Type     string
	Variadic bool
	Tag      string
	Embedded bool
}

type DependencyTypeMethod struct {
	Name       string
	Receiver   string
	Signature  string
	Variadic   bool
	FunctionID string
	ModulePath string
	TypeParams []DependencyTypeParameter
}

type DependencyTypeParameter struct {
	Name       string
	Binding    string
	Constraint string
}

// DependencyPackage describes a resolved package, including one with no exports.
// Members may include private type metadata needed to describe public APIs;
// source lookup independently enforces exported-name visibility.
type DependencyPackage struct {
	ModulePath string
	Members    []DependencyExport
}

type AnalyzeOptions struct {
	Limits       ast.Limits
	Dependencies []DependencyPackage
}

func (a *analyzer) importedMember(modulePath, qualifier, name string, span source.Span) (DependencyExport, bool) {
	if _, resolved := a.dependencies[modulePath]; !resolved {
		return DependencyExport{}, false // The import declaration owns this diagnostic.
	}
	code := "semantic.import.member.missing"
	if !isExported(name) && modulePath != a.definitionModule {
		code = "semantic.import.member.unexported"
	} else if member, ok := a.dependency(modulePath, name); ok {
		return member, true
	}
	a.addDiagnostic(code, fmt.Sprintf("%s.%s: package %q has no accessible member %q", qualifier, name, modulePath, name), span)
	return DependencyExport{}, false
}

func (a *analyzer) registerDependencies(exports []DependencyExport) {
	for i := range exports {
		export := exports[i]
		export.constantValue = nil
		export.ModulePath = strings.TrimSpace(export.ModulePath)
		export.Name = strings.TrimSpace(export.Name)
		if export.ModulePath == "" || export.Name == "" {
			continue
		}
		if !export.Kind.dependencyKind() {
			a.addDiagnostic("semantic.dependency.kind", "dependency export has invalid kind", source.Span{})
			continue
		}
		if a.dependencies[export.ModulePath] == nil {
			a.dependencies[export.ModulePath] = make(map[string]DependencyExport)
		}
		a.dependencies[export.ModulePath][export.Name] = export
		if export.Kind != ObjectType {
			continue
		}
		key := types.TypeKey{ModulePath: export.ModulePath, DeclID: types.DeclID(export.Name)}
		if _, exists := a.info.TypeTable.Named(key); exists {
			continue
		}
		_ = a.info.TypeTable.Add(types.TypeNode{
			ID:   types.TypeID("dependency." + export.ModulePath + "." + export.Name),
			Kind: types.Named, Identity: key,
		})
	}
	modulePaths := make([]string, 0, len(a.dependencies))
	for modulePath := range a.dependencies {
		modulePaths = append(modulePaths, modulePath)
	}
	sort.Strings(modulePaths)
	for _, modulePath := range modulePaths {
		module := a.dependencies[modulePath]
		names := make([]string, 0, len(module))
		for name := range module {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			export := module[name]
			if export.Kind != ObjectType {
				continue
			}
			key := types.TypeKey{ModulePath: modulePath, DeclID: types.DeclID(export.Name)}
			node, ok := a.info.TypeTable.Named(key)
			if !ok {
				continue
			}
			parser := a.dependencyParser(export)
			self := modulePath + "." + export.Name
			if target := parseDependencyType(parser, export.Type); target.Valid() && strings.TrimSpace(export.Type) != self {
				node.Alias = true
				node.AliasTarget = target
			} else if underlying := parseDependencyType(parser, export.Underlying); underlying.Valid() {
				node.Underlying = underlying
			}
			for _, field := range export.Fields {
				fieldType := parseDependencyType(parser, field.Type)
				if fieldType.Valid() {
					node.Fields = append(node.Fields, types.Field{
						Name: strings.TrimSpace(field.Name), Type: fieldType,
						Tag: field.Tag, Embedded: field.Embedded,
					})
				}
			}
			for _, method := range export.Methods {
				methodParser := a.dependencyParser(export)
				objectID := ObjectID("dependency.method." + modulePath + "." + export.Name + "." + method.Name)
				if len(method.TypeParams) != 0 {
					if methodParser.Bindings == nil {
						methodParser.Bindings = make(map[string]types.TypeRef)
					}
					for _, param := range method.TypeParams {
						id := types.TypeID(string(objectID) + "." + param.Name)
						ref := types.TypeRef{Kind: types.TypeParameter, Node: id}
						_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.TypeParameter, Name: param.Name, Constraint: types.AnyType()})
						methodParser.Bindings[param.Binding] = ref
						paramID := ObjectID(id)
						a.info.Objects[paramID] = Object{ID: paramID, Kind: ObjectTypeParam, Name: param.Name, Type: ref}
						a.info.GenericDecls[objectID] = append(a.info.GenericDecls[objectID], paramID)
					}
					for _, param := range method.TypeParams {
						node, _ := a.info.TypeTable.Node(methodParser.Bindings[param.Binding])
						if constraint := parseDependencyType(methodParser, param.Constraint); constraint.Valid() {
							node.Constraint = constraint
							_ = a.info.TypeTable.Replace(node)
						}
					}
				}
				methodType := parseDependencyType(methodParser, method.Signature)
				signature, ok := a.info.TypeTable.IsFunction(methodType)
				if !ok {
					continue
				}
				signature.Variadic = signature.Variadic || method.Variadic
				resolved := types.Method{
					Name: strings.TrimSpace(method.Name), Receiver: parseDependencyType(methodParser, method.Receiver),
					Signature: signature, FunctionID: strings.TrimSpace(method.FunctionID),
					ModulePath: firstNonEmpty(method.ModulePath, modulePath),
				}
				if len(method.TypeParams) != 0 {
					a.info.Objects[objectID] = Object{ID: objectID, Kind: ObjectFunc, Name: method.Name, Type: methodType, ModulePath: modulePath, Exported: isExported(method.Name)}
					a.info.GenericMethods = append(a.info.GenericMethods, GenericMethod{Method: resolved, Object: objectID})
				} else {
					node.Methods = append(node.Methods, resolved)
				}
			}
			_ = a.info.TypeTable.Replace(node)
		}
	}
}

func parseDependencyType(parser *types.Parser, text string) types.TypeRef {
	text = strings.TrimSpace(text)
	if text == "" {
		return types.TypeRef{}
	}
	ref, err := parser.Parse(text)
	if err != nil {
		return types.TypeRef{}
	}
	return ref
}

func (a *analyzer) dependency(modulePath, name string) (DependencyExport, bool) {
	module := a.dependencies[strings.TrimSpace(modulePath)]
	if len(module) == 0 {
		return DependencyExport{}, false
	}
	export, ok := module[strings.TrimSpace(name)]
	return export, ok
}

func (a *analyzer) dependencyParser(export DependencyExport) *types.Parser {
	parser := types.NewParser(export.ModulePath, a.info.TypeTable)
	if len(export.TypeParams) != 0 {
		parser.Bindings = make(map[string]types.TypeRef, len(export.TypeParams))
		for _, name := range export.TypeParams {
			id := types.TypeID("dependency.parameter." + export.ModulePath + "." + export.Name + "." + name)
			ref := types.TypeRef{Kind: types.TypeParameter, Node: id}
			if _, exists := a.info.TypeTable.Node(ref); !exists {
				_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.TypeParameter, Name: name, Constraint: types.AnyType()})
			}
			parser.Bindings[name] = ref
		}
	}
	return parser
}

func (a *analyzer) dependencyType(export DependencyExport) types.TypeRef {
	if export.Kind == ObjectType {
		if node, ok := a.info.TypeTable.Named(types.TypeKey{ModulePath: export.ModulePath, DeclID: types.DeclID(export.Name)}); ok {
			return types.TypeRef{Kind: types.Named, Node: node.ID, Named: node.Identity}
		}
	}
	return parseDependencyType(a.dependencyParser(export), export.Type)
}
