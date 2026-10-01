package compiler

import (
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/parser"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func checkWorkspace(request Request, roots []string) (CheckResult, error) {
	analyzed, err := analyzePackages(AnalysisRequest{Request: request, Roots: roots}, false)
	if err != nil {
		return CheckResult{}, err
	}
	documents := make(map[string][]parser.Document, len(analyzed.Packages))
	for path, pkg := range analyzed.Packages {
		documents[path] = pkg.Documents
	}
	return CheckResult{
		Target: analyzed.Target, Documents: documents, ExportHashes: analyzed.ExportHashes,
		Order: analyzed.Order, GraphHash: analyzed.GraphHash, Diagnostics: analyzed.Diagnostics, Stats: analyzed.Stats,
	}, nil
}

// DependencyExports returns the deterministic public semantic surface consumed
// when checking packages that import info's package.
func DependencyExports(info *check.ProgramInfo) []check.DependencyExport {
	if info == nil {
		return nil
	}
	scope := info.Scope(info.PackageScope)
	if scope == nil {
		return nil
	}
	names := make([]string, 0, len(scope.Objects))
	for name := range scope.Objects {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]check.DependencyExport, 0, len(names))
	for _, name := range names {
		object, ok := info.Object(scope.Objects[name])
		if !ok || (!object.Exported && object.Kind != check.ObjectType) {
			continue
		}
		export := check.DependencyExport{
			ModulePath: info.ModulePath, Name: name,
			Type: types.FormatWithTable(info.TypeTable, object.Type), Untyped: object.Untyped,
		}
		for _, param := range info.GenericDecls[object.ID] {
			export.TypeParams = append(export.TypeParams, info.Objects[param].Name)
		}
		switch object.Kind {
		case check.ObjectConst:
			export.Kind, export.ID = check.ObjectConst, "const."+name
			if value, ok := info.ConstObjects[object.ID]; ok {
				export.Value = value.JSON()
			}
		case check.ObjectVar:
			export.Kind, export.ID = check.ObjectVar, "global."+name
		case check.ObjectFunc:
			export.Kind, export.ID = check.ObjectFunc, firstNonEmpty(object.FunctionID, "fn."+name)
			if signature, ok := info.TypeTable.IsFunction(object.Type); ok {
				export.Variadic = signature.Variadic
			}
		case check.ObjectType:
			export.Kind, export.ID = check.ObjectType, "type."+name
			node, ok := info.TypeTable.Node(object.Type)
			if !ok {
				continue
			}
			if node.Alias {
				export.Type = types.FormatWithTable(info.TypeTable, node.AliasTarget)
			} else {
				export.Underlying = types.FormatWithTable(info.TypeTable, node.Underlying)
			}
			for _, field := range node.Fields {
				export.Fields = append(export.Fields, check.DependencyTypeField{
					Name: field.Name, Type: types.FormatWithTable(info.TypeTable, field.Type),
					Tag: field.Tag, Embedded: field.Embedded,
				})
			}
			for _, method := range node.Methods {
				export.Methods = append(export.Methods, check.DependencyTypeMethod{
					Name: method.Name, Receiver: types.FormatWithTable(info.TypeTable, method.Receiver),
					Signature: types.FormatSignature(info.TypeTable, method.Signature),
					Variadic:  method.Signature.Variadic, FunctionID: method.FunctionID,
					ModulePath: firstNonEmpty(method.ModulePath, info.ModulePath),
				})
			}
			for _, generic := range info.GenericMethods {
				receiver := generic.Method.Receiver
				if receiver.Kind == types.Pointer {
					receiver, _ = info.Relations.View(receiver).Elem()
				}
				if instance, ok := info.TypeTable.Node(receiver); ok && instance.Kind == types.Instance {
					receiver = instance.Base
				}
				if !info.Relations.Identical(receiver, object.Type).OK {
					continue
				}
				export.Methods = append(export.Methods, genericMethodDependency(info, generic))
			}
		default:
			continue
		}
		if strings.TrimSpace(export.Type) == "" {
			export.Type = "Any"
		}
		out = append(out, export)
	}
	return out
}

// Generic parameter names must not shadow canonical primitive names such as
// Int at the dependency text boundary. The source AST retains the user's names.
func genericMethodDependency(info *check.ProgramInfo, generic check.GenericMethod) check.DependencyTypeMethod {
	table := types.NewTable(info.TypeTable.Nodes...)
	params := info.GenericDecls[generic.Object]
	for i, id := range params {
		node, _ := table.Node(info.Objects[id].Type)
		node.Name = "_method_parameter_" + strconv.Itoa(i)
		_ = table.Replace(node)
	}
	method := check.DependencyTypeMethod{Name: generic.Method.Name, Receiver: types.FormatWithTable(table, generic.Method.Receiver), Signature: types.FormatSignature(table, generic.Method.Signature), Variadic: generic.Method.Signature.Variadic, ModulePath: info.ModulePath}
	for _, id := range params {
		param, _ := table.Node(info.Objects[id].Type)
		constraint := types.View(table, param.Constraint).Underlying()
		method.TypeParams = append(method.TypeParams, check.DependencyTypeParameter{Name: info.Objects[id].Name, Binding: param.Name, Constraint: types.FormatWithTable(table, constraint)})
	}
	return method
}
