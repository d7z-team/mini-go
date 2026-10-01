// Package analysis projects checked source facts for developer tools.
package analysis

import (
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
	"github.com/d7z-team/mini-go/compiler/types"
)

type SymbolKey struct {
	ModulePath string
	Kind       check.ObjectKind
	Name       string
	Receiver   string
}

func (k SymbolKey) Valid() bool { return k.ModulePath != "" && k.Name != "" }

type Occurrence struct {
	Name     ast.Identifier
	Node     ast.NodeID
	Role     ast.NameRole
	Object   check.ObjectID
	Symbol   SymbolKey
	Type     types.TypeRef
	TypeText string
}

type Package struct {
	Checked     check.CheckedProgram
	Documents   []parser.Document
	Occurrences []Occurrence
	Diagnostics []source.Diagnostic
}

type PublicAPI struct {
	ModulePath   string              `json:"module_path"`
	Package      string              `json:"package"`
	Declarations []PublicDeclaration `json:"declarations"`
}

type PublicDeclaration struct {
	Kind       string        `json:"kind"`
	Name       string        `json:"name"`
	Type       string        `json:"type"`
	Alias      bool          `json:"alias,omitempty"`
	Untyped    bool          `json:"untyped,omitempty"`
	Exact      string        `json:"exact,omitempty"`
	Fields     []PublicField `json:"fields,omitempty"`
	Methods    []PublicField `json:"methods,omitempty"`
	TypeParams []PublicField `json:"type_params,omitempty"`
}

type PublicField struct {
	Name       string        `json:"name,omitempty"`
	Type       string        `json:"type"`
	Tag        string        `json:"tag,omitempty"`
	Embedded   bool          `json:"embedded,omitempty"`
	Variadic   bool          `json:"variadic,omitempty"`
	TypeParams []PublicField `json:"type_params,omitempty"`
}

// ProjectPublicAPI returns the deterministic exported declaration surface of
// one semantically checked package.
func ProjectPublicAPI(source Package) PublicAPI {
	info := source.Checked.Info
	out := PublicAPI{ModulePath: info.ModulePath, Package: info.Package}
	scope := info.Scopes[info.PackageScope]
	if scope == nil {
		return out
	}
	for _, objectID := range scope.Objects {
		object := info.Objects[objectID]
		if !object.Exported || !exportedName(object.Name) {
			continue
		}
		decl := PublicDeclaration{Name: object.Name, Type: types.FormatWithTable(info.TypeTable, object.Type), Alias: object.Alias, Untyped: object.Untyped}
		switch object.Kind {
		case check.ObjectConst:
			decl.Kind = "const"
			if value, ok := info.ConstObjects[object.ID]; ok {
				decl.Exact = value.ExactText()
			}
		case check.ObjectVar:
			decl.Kind = "var"
		case check.ObjectFunc:
			decl.Kind = "func"
		case check.ObjectType:
			decl.Kind = "type"
			_, ok := info.TypeTable.Node(object.Type)
			if !ok {
				_, ok = info.TypeTable.Named(object.Type.Named)
			}
			if ok {
				shape := info.TypeTable.Underlying(object.Type)
				if underlying, found := info.TypeTable.Node(shape); found {
					for _, field := range underlying.Fields {
						if exportedName(field.Name) {
							decl.Fields = append(decl.Fields, PublicField{Name: field.Name, Type: types.FormatWithTable(info.TypeTable, field.Type), Tag: field.Tag, Embedded: field.Embedded})
						}
					}
				}
				methods := publicMethodSet(info.TypeTable, object.Type, map[types.TypeID]bool{})
				if underlying, found := info.TypeTable.IsInterface(object.Type); found {
					methods = underlying.Methods
				}
				for _, method := range methods {
					if exportedName(method.Name) {
						decl.Methods = append(decl.Methods, PublicField{Name: method.Name, Type: types.FormatSignature(info.TypeTable, method.Signature), Variadic: method.Signature.Variadic})
					}
				}
				for _, generic := range info.GenericMethods {
					receiver := generic.Method.Receiver
					if receiver.Kind == types.Pointer {
						receiver, _ = info.Relations.View(receiver).Elem()
					}
					if instance, found := info.TypeTable.Node(receiver); found && instance.Kind == types.Instance {
						receiver = instance.Base
					}
					if !exportedName(generic.Method.Name) || !info.Relations.Identical(receiver, object.Type).OK {
						continue
					}
					method := PublicField{Name: generic.Method.Name, Type: types.FormatSignature(info.TypeTable, generic.Method.Signature), Variadic: generic.Method.Signature.Variadic}
					for _, id := range info.GenericDecls[generic.Object] {
						param := info.Objects[id]
						constraint, _ := info.Relations.View(param.Type).Constraint()
						method.TypeParams = append(method.TypeParams, PublicField{Name: param.Name, Type: types.FormatWithTable(info.TypeTable, constraint)})
					}
					decl.Methods = append(decl.Methods, method)
				}
			}
		default:
			continue
		}
		for _, paramID := range info.GenericDecls[object.ID] {
			param, ok := info.Objects[paramID]
			if !ok {
				continue
			}
			constraint := types.AnyType()
			if node, found := info.TypeTable.Node(param.Type); found && node.Constraint.Valid() {
				constraint = node.Constraint
			}
			decl.TypeParams = append(decl.TypeParams, PublicField{Name: param.Name, Type: types.FormatWithTable(info.TypeTable, constraint)})
		}
		sort.Slice(decl.Methods, func(i, j int) bool { return decl.Methods[i].Name < decl.Methods[j].Name })
		out.Declarations = append(out.Declarations, decl)
	}
	sort.Slice(out.Declarations, func(i, j int) bool {
		if out.Declarations[i].Name != out.Declarations[j].Name {
			return out.Declarations[i].Name < out.Declarations[j].Name
		}
		return out.Declarations[i].Kind < out.Declarations[j].Kind
	})
	return out
}

type methodCandidate struct {
	method types.Method
	depth  int
	count  int
}

func publicMethodSet(table *types.TypeTable, ref types.TypeRef, seen map[types.TypeID]bool) []types.Method {
	if table == nil || !ref.Valid() {
		return nil
	}
	if ref.Kind == types.Pointer {
		if node, ok := table.Node(ref); ok {
			ref = node.Elem
		}
	}
	node, ok := table.Node(ref)
	if !ok && ref.Kind == types.Named {
		node, ok = table.Named(ref.Named)
	}
	if !ok || seen[node.ID] {
		return nil
	}
	seen[node.ID] = true
	defer delete(seen, node.ID)
	candidates := map[string]methodCandidate{}
	for _, method := range node.Methods {
		candidates[method.Name] = methodCandidate{method: method, count: 1}
	}
	underlying := table.Underlying(ref)
	structure, ok := table.Node(underlying)
	if !ok || structure.Kind != types.Struct {
		out := make([]types.Method, 0, len(candidates))
		for _, candidate := range candidates {
			out = append(out, candidate.method)
		}
		return out
	}
	for _, field := range structure.Fields {
		if !field.Embedded {
			continue
		}
		for _, method := range publicMethodSet(table, field.Type, seen) {
			candidate, exists := candidates[method.Name]
			if !exists {
				candidates[method.Name] = methodCandidate{method: method, depth: 1, count: 1}
			} else if candidate.depth == 1 {
				candidate.count++
				candidates[method.Name] = candidate
			}
		}
	}
	out := make([]types.Method, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.count == 1 {
			out = append(out, candidate.method)
		}
	}
	return out
}

func exportedName(name string) bool {
	return token.IsExportedName(name)
}

func CheckProgram(program ast.Program, options check.AnalyzeOptions) Package {
	checked := check.WithOptions(program, options)
	return Package{Checked: checked, Diagnostics: append([]source.Diagnostic(nil), checked.Info.Diagnostics...)}
}

// Index projects tooling occurrences from an already checked package without
// repeating scanner, parser, or semantic analysis.
func Index(source Package) Package {
	source.Occurrences = nil
	checked := source.Checked
	check.BindSourceNames(checked.Program, checked.Info)
	for _, name := range ast.Names(checked.Program) {
		occurrence := Occurrence{Name: name.Name, Node: name.Node, Role: name.Role}
		if objectID := checked.Info.NameDefs[name.Name.ID]; objectID != "" {
			occurrence.Object = objectID
		} else if objectID := checked.Info.NameUses[name.Name.ID]; objectID != "" {
			occurrence.Object = objectID
		}
		if object, ok := checked.Info.Objects[occurrence.Object]; ok {
			occurrence.Type = object.Type
			occurrence.Symbol = objectSymbol(checked.Info.ModulePath, object)
		}
		if selection, ok := checked.Info.Selections[name.Node]; ok {
			occurrence.Type = selection.Type
			if selection.Object != "" {
				occurrence.Object = selection.Object
			}
			if selection.ModulePath != "" && selection.Name != "" {
				occurrence.Symbol = SymbolKey{ModulePath: selection.ModulePath, Name: selection.Name}
			}
		}
		if !occurrence.Type.Valid() {
			if expr, ok := checked.Info.Exprs[name.Node]; ok {
				occurrence.Type = expr.Type
			}
			if typ, ok := checked.Info.Types[name.Node]; ok && !occurrence.Type.Valid() {
				occurrence.Type = typ.Type
			}
		}
		if occurrence.Type.Valid() {
			occurrence.TypeText = types.FormatWithTable(checked.Info.TypeTable, occurrence.Type)
		}
		if parameters := checked.Info.GenericDecls[occurrence.Object]; len(parameters) != 0 {
			var params []string
			for _, id := range parameters {
				param := checked.Info.Objects[id]
				constraint, _ := checked.Info.Relations.View(param.Type).Constraint()
				params = append(params, param.Name+" "+types.FormatWithTable(checked.Info.TypeTable, constraint))
			}
			occurrence.TypeText = "[" + strings.Join(params, ", ") + "] " + occurrence.TypeText
		}
		source.Occurrences = append(source.Occurrences, occurrence)
	}
	sort.SliceStable(source.Occurrences, func(i, j int) bool {
		left, right := source.Occurrences[i].Name.Span, source.Occurrences[j].Name.Span
		if left.Start.File != right.Start.File {
			return left.Start.File < right.Start.File
		}
		if left.Start.Offset != right.Start.Offset {
			return left.Start.Offset < right.Start.Offset
		}
		return source.Occurrences[i].Name.ID < source.Occurrences[j].Name.ID
	})
	return source
}

func objectSymbol(modulePath string, object check.Object) SymbolKey {
	owner := strings.TrimSpace(object.ModulePath)
	if owner == "" {
		owner = modulePath
	}
	name := object.Name
	if object.ExportName != "" {
		name = object.ExportName
	}
	return SymbolKey{ModulePath: owner, Kind: object.Kind, Name: name}
}
