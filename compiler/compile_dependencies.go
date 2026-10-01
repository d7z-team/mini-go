package compiler

import (
	"fmt"
	"strings"

	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func finalizeDirectArtifactRequirements(modulePath string, artifact *ir.Artifact, dependencyHashes map[string]string, diagnostics *[]source.Diagnostic) bool {
	artifact.Requirements = append([]ir.Requirement(nil), artifact.Requirements...)
	ok := true
	for i, requirement := range artifact.Requirements {
		if requirement.Kind != ir.RequirementSource {
			continue
		}
		dependencyPath := strings.TrimSpace(requirement.ModulePath)
		hash := strings.TrimSpace(dependencyHashes[dependencyPath])
		if hash == "" {
			*diagnostics = append(*diagnostics, diagnostic("compiler.workspace.module.missing", fmt.Sprintf("missing module %q required by %q", dependencyPath, modulePath)))
			ok = false
			continue
		}
		artifact.Requirements[i].Hash = hash
	}
	return ok
}

func dependencyPackages(imports []string, artifacts map[string]ir.Artifact) []check.DependencyPackage {
	return semanticDependencies(imports, func(modulePath string) ([]check.DependencyExport, []string, bool) {
		artifact, ok := artifacts[modulePath]
		if !ok {
			return nil, nil, false
		}
		var out []check.DependencyExport
		for _, export := range artifact.Exports {
			out = append(out, dependencyExportInfo(modulePath, artifact, export, artifacts))
		}
		appendDependencyPrivateTypeMetadata(modulePath, artifact, &out)
		var dependencies []string
		for _, requirement := range artifact.Requirements {
			if requirement.Kind == ir.RequirementSource {
				dependencies = append(dependencies, strings.TrimSpace(requirement.ModulePath))
			}
		}
		return out, dependencies, true
	})
}

func dependencyExportInfo(modulePath string, artifact ir.Artifact, export ir.Export, artifacts map[string]ir.Artifact) check.DependencyExport {
	kind := dependencyExportKind(export.Kind)
	typeNames := artifactTypeNames(artifact)
	exportType := qualifyDependencyTypeRef(modulePath, export.Type, &artifact.TypeTable, typeNames)
	underlying := ""
	var fields []check.DependencyTypeField
	var methods []check.DependencyTypeMethod
	variadic := false
	if kind == check.ObjectFunc {
		if signature, ok := artifact.TypeTable.IsFunction(export.Type); ok {
			variadic = signature.Variadic
		}
	}
	if kind == check.ObjectType {
		if decl, ok := artifactTypeDecl(artifact, export.ID); ok {
			underlying = qualifyDependencyTypeRef(modulePath, decl.Underlying, &artifact.TypeTable, typeNames)
			fields = dependencyTypeFields(modulePath, artifactTypeFields(artifact, decl), &artifact.TypeTable, typeNames)
			methods = dependencyTypeMethods(modulePath, decl.Methods, &artifact.TypeTable, typeNames)
			if signature, ok := artifact.TypeTable.IsFunction(types.Ref(decl)); ok {
				variadic = signature.Variadic
			}
		} else if declModulePath, decl, ok := dependencyAliasTypeDecl(types.FormatWithTable(&artifact.TypeTable, export.Type), artifacts); ok {
			declTypeNames := artifactTypeNames(artifacts[declModulePath])
			declArtifact := artifacts[declModulePath]
			underlying = qualifyDependencyTypeRef(declModulePath, decl.Underlying, &declArtifact.TypeTable, declTypeNames)
			fields = dependencyTypeFields(declModulePath, artifactTypeFields(declArtifact, decl), &declArtifact.TypeTable, declTypeNames)
			methods = dependencyTypeMethods(declModulePath, decl.Methods, &declArtifact.TypeTable, declTypeNames)
			if signature, ok := declArtifact.TypeTable.IsFunction(types.Ref(decl)); ok {
				variadic = signature.Variadic
			}
		}
	}
	var value []byte
	if kind == check.ObjectConst {
		if constant, ok := artifactConstant(artifact, export.ID); ok {
			value = append(value, constant.Value...)
		}
	}
	return check.DependencyExport{
		ModulePath: modulePath,
		Name:       strings.TrimSpace(export.Name),
		ID:         strings.TrimSpace(export.ID),
		Kind:       kind,
		Type:       exportType,
		Underlying: underlying,
		Value:      value,
		Fields:     fields,
		Methods:    methods,
		Variadic:   variadic,
		Untyped:    export.Untyped,
	}
}

func dependencyExportKind(kind string) check.ObjectKind {
	switch strings.TrimSpace(kind) {
	case "const":
		return check.ObjectConst
	case "global":
		return check.ObjectVar
	case "type":
		return check.ObjectType
	case "function":
		return check.ObjectFunc
	default:
		return check.ObjectInvalid
	}
}

func artifactConstant(artifact ir.Artifact, id string) (ir.Constant, bool) {
	id = strings.TrimSpace(id)
	for _, constant := range artifact.Constants {
		if strings.TrimSpace(constant.ID) == id {
			return constant, true
		}
	}
	return ir.Constant{}, false
}

func appendDependencyPrivateTypeMetadata(modulePath string, artifact ir.Artifact, out *[]check.DependencyExport) {
	typeNames := artifactTypeNames(artifact)
	exportedTypes := map[string]struct{}{}
	for _, export := range artifact.Exports {
		if export.Kind == "type" {
			exportedTypes[strings.TrimSpace(export.Name)] = struct{}{}
		}
	}
	for _, decl := range artifact.TypeTable.DefinedNamed(artifact.Module.Path) {
		name := strings.TrimSpace(string(decl.Identity.DeclID))
		if name == "" {
			continue
		}
		if _, exported := exportedTypes[name]; exported {
			continue
		}
		*out = append(*out, check.DependencyExport{
			ModulePath: modulePath,
			Name:       name,
			Kind:       check.ObjectType,
			Type:       modulePath + "." + name,
			Underlying: qualifyDependencyTypeRef(modulePath, decl.Underlying, &artifact.TypeTable, typeNames),
			Fields:     dependencyTypeFields(modulePath, artifactTypeFields(artifact, decl), &artifact.TypeTable, typeNames),
			Methods:    dependencyTypeMethods(modulePath, decl.Methods, &artifact.TypeTable, typeNames),
			Variadic:   artifactTypeVariadic(artifact, decl),
		})
	}
}

func dependencyTypeFields(modulePath string, fields []types.Field, table *types.TypeTable, typeNames map[string]struct{}) []check.DependencyTypeField {
	out := make([]check.DependencyTypeField, 0, len(fields))
	for _, field := range fields {
		variadic := false
		if signature, ok := table.IsFunction(field.Type); ok {
			variadic = signature.Variadic
		}
		out = append(out, check.DependencyTypeField{
			Name:     strings.TrimSpace(field.Name),
			Type:     qualifyDependencyTypeRef(modulePath, field.Type, table, typeNames),
			Variadic: variadic,
			Tag:      field.Tag,
			Embedded: field.Embedded,
		})
	}
	return out
}

func dependencyTypeMethods(modulePath string, methods []types.Method, table *types.TypeTable, typeNames map[string]struct{}) []check.DependencyTypeMethod {
	out := make([]check.DependencyTypeMethod, 0, len(methods))
	for _, method := range methods {
		methodModulePath := strings.TrimSpace(method.ModulePath)
		if methodModulePath == "" {
			methodModulePath = modulePath
		}
		out = append(out, check.DependencyTypeMethod{
			Name:       strings.TrimSpace(method.Name),
			Receiver:   qualifyDependencyTypeRef(modulePath, method.Receiver, table, typeNames),
			Signature:  qualifyDependencyType(modulePath, types.FormatSignature(table, method.Signature), typeNames),
			Variadic:   method.Signature.Variadic,
			FunctionID: strings.TrimSpace(method.FunctionID),
			ModulePath: methodModulePath,
		})
	}
	return out
}

func dependencyAliasTypeDecl(exportType string, artifacts map[string]ir.Artifact) (string, types.TypeNode, bool) {
	modulePath, typeName, ok := splitQualifiedTypeName(exportType)
	if !ok {
		return "", types.TypeNode{}, false
	}
	artifact, ok := artifacts[modulePath]
	if !ok {
		return "", types.TypeNode{}, false
	}
	decl, ok := artifactTypeDecl(artifact, "type."+typeName)
	return modulePath, decl, ok
}

func splitQualifiedTypeName(typ string) (string, string, bool) {
	typ = strings.TrimSpace(typ)
	dot := strings.LastIndex(typ, ".")
	if dot <= 0 || dot == len(typ)-1 {
		return "", "", false
	}
	return strings.TrimSpace(typ[:dot]), strings.TrimSpace(typ[dot+1:]), true
}

func artifactTypeNames(artifact ir.Artifact) map[string]struct{} {
	out := map[string]struct{}{}
	for _, decl := range artifact.TypeTable.DefinedNamed(artifact.Module.Path) {
		if name := strings.TrimSpace(string(decl.Identity.DeclID)); name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

func artifactTypeDecl(artifact ir.Artifact, id string) (types.TypeNode, bool) {
	id = strings.TrimSpace(id)
	name := strings.TrimPrefix(id, "type.")
	for _, decl := range artifact.TypeTable.DefinedNamed(artifact.Module.Path) {
		if string(decl.Identity.DeclID) == name || string(decl.ID) == id {
			return decl, true
		}
	}
	return types.TypeNode{}, false
}

func artifactTypeFields(artifact ir.Artifact, decl types.TypeNode) []types.Field {
	shape, ok := artifact.TypeTable.Node(artifact.TypeTable.Underlying(types.Ref(decl)))
	if !ok || shape.Kind != types.Struct {
		return nil
	}
	return shape.Fields
}

func artifactTypeVariadic(artifact ir.Artifact, decl types.TypeNode) bool {
	signature, ok := artifact.TypeTable.IsFunction(types.Ref(decl))
	return ok && signature.Variadic
}

func qualifyDependencyTypeRef(modulePath string, ref types.TypeRef, table *types.TypeTable, typeNames map[string]struct{}) string {
	if !ref.Valid() {
		return ""
	}
	return qualifyDependencyType(modulePath, types.FormatWithTable(table, ref), typeNames)
}

func qualifyDependencyType(modulePath, typ string, typeNames map[string]struct{}) string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return ""
	}
	qualified, ok := types.RewriteCanonicalText(typ, dependencyTypeNameQualifier(modulePath, typeNames))
	if ok {
		return qualified
	}
	return typ
}

func dependencyTypeNameQualifier(modulePath string, typeNames map[string]struct{}) func(string) (string, bool) {
	modulePath = strings.TrimSpace(modulePath)
	return func(name string) (string, bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", false
		}
		if _, ok := typeNames[name]; ok {
			return modulePath + "." + name, true
		}
		return "", false
	}
}
