package compiler

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/cache"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/token"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func buildPackageData(program ast.Program, artifact ir.Artifact, artifactHash string, symbols ir.PackageSymbols, info *check.ProgramInfo) (cache.PackageData, error) {
	data := cache.PackageData{
		Format: cache.ExportFormat, Version: cache.ExportVersion,
		ModulePath: artifact.Module.Path, Package: artifact.Module.Package,
		ArtifactHash: artifactHash, TypeTable: artifact.TypeTable,
		Constants:    append([]ir.Constant(nil), artifact.Constants...),
		Exports:      append([]ir.Export(nil), artifact.Exports...),
		Requirements: append([]ir.Requirement(nil), artifact.Requirements...),
	}
	data.SourceFiles = append([]ir.SourceFile(nil), symbols.Files...)
	genericTypes := make(map[string][]ast.TypeParam)
	for i := range program.Files {
		for j := range program.Files[i].Decls {
			decl := program.Files[i].Decls[j]
			switch {
			case decl.Kind == ast.DeclType && len(decl.Type.TypeParams) != 0:
				name := decl.Type.Name
				genericTypes[name] = decl.Type.TypeParams
				if isExported(name) {
					data.GenericTemplates = append(data.GenericTemplates, cache.GenericTemplate{DeclID: types.DeclID(name), Kind: "type", Name: name, Decl: decl})
				}
			case decl.Kind == ast.DeclFunc && decl.Func.Receiver == nil && len(decl.Func.TypeParams) != 0 && isExported(decl.Func.Name):
				data.GenericTemplates = append(data.GenericTemplates, cache.GenericTemplate{DeclID: types.DeclID(decl.Func.Name), Kind: "function", Name: decl.Func.Name, Decl: decl})
			}
		}
	}
	for i := range program.Files {
		for j := range program.Files[i].Decls {
			decl := program.Files[i].Decls[j]
			if decl.Kind != ast.DeclFunc || decl.Func.Receiver == nil {
				continue
			}
			methodName := decl.Func.Name
			receiverName := genericReceiverName(decl.Func.Receiver.Type)
			if (len(genericTypes[receiverName]) != 0 || len(decl.Func.TypeParams) != 0) && isExported(receiverName) && isExported(methodName) {
				name := receiverName + "." + methodName
				data.GenericTemplates = append(data.GenericTemplates, cache.GenericTemplate{DeclID: types.DeclID(name), Kind: "method", Name: name, Decl: decl})
			}
		}
	}
	declarations := make(map[string]ast.Decl)
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclFunc && decl.Func.Receiver == nil && len(decl.Func.TypeParams) != 0 {
				declarations[decl.Func.Name] = decl
			}
			if decl.Kind == ast.DeclType && len(decl.Type.TypeParams) != 0 {
				declarations[decl.Type.Name] = decl
			}
		}
	}
	included := make(map[string]bool)
	for _, template := range data.GenericTemplates {
		included[template.Name] = true
	}
	for index := 0; index < len(data.GenericTemplates); index++ {
		template := &data.GenericTemplates[index]
		template.References = genericTemplateReferences(template.Decl, info)
		body := ast.Program{Files: []ast.File{{Decls: []ast.Decl{template.Decl}}}}
		ast.WalkExpressions(&body, func(expr *ast.Expression) {
			fact := info.Exprs[expr.NodeID]
			if expr.Type == nil && fact.Type.Valid() && fact.Mode != check.ExprPackage && !fact.Untyped {
				template.Expressions = append(template.Expressions, cache.GenericExpression{Node: expr.NodeID, Type: fact.Type})
			}
		})
		for _, generic := range info.GenericMethods {
			if template.Decl.Func == nil || info.Objects[generic.Object].Node != template.Decl.Func.NodeID {
				continue
			}
			method := genericMethodDependency(info, generic)
			template.Type, template.Receiver = method.Signature, method.Receiver
			for _, param := range method.TypeParams {
				template.TypeParams = append(template.TypeParams, cache.GenericTypeParameter{Name: param.Name, Binding: param.Binding, Constraint: param.Constraint})
			}
		}
		if object, ok := info.Lookup(info.PackageScope, template.Name); ok {
			template.Type = types.FormatWithTable(info.TypeTable, object.Type)
		}
		for _, reference := range template.References {
			if !reference.Generic || included[reference.Name] {
				continue
			}
			decl, found := declarations[reference.Name]
			if !found {
				continue
			}
			included[reference.Name] = true
			kind := "function"
			if decl.Kind == ast.DeclType {
				kind = "type"
			}
			data.GenericTemplates = append(data.GenericTemplates, cache.GenericTemplate{DeclID: types.DeclID(reference.Name), Kind: kind, Name: reference.Name, Decl: decl})
		}
	}
	if len(data.GenericTemplates) != 0 {
		referenced := make(map[string]struct{}, len(data.GenericTemplates))
		for _, template := range data.GenericTemplates {
			if path := strings.TrimSpace(template.Decl.Span.Start.File); path != "" {
				referenced[path] = struct{}{}
			}
		}
		files := data.SourceFiles[:0]
		for _, file := range data.SourceFiles {
			if _, ok := referenced[strings.TrimSpace(file.ID)]; !ok {
				if _, ok = referenced[strings.TrimSpace(file.Path)]; !ok {
					continue
				}
			}
			files = append(files, file)
		}
		data.SourceFiles = files
	} else {
		data.SourceFiles = nil
	}
	if len(data.GenericTemplates) != 0 {
		for _, node := range info.TypeTable.Nodes {
			if _, exists := data.TypeTable.Node(types.TypeRef{Kind: node.Kind, Node: node.ID}); exists {
				continue
			}
			if err := data.TypeTable.Add(node); err != nil {
				return cache.PackageData{}, fmt.Errorf("merge compiler type %q: %w", node.ID, err)
			}
		}
	}
	var err error
	data.ExportHash, err = data.Hash()
	if err != nil {
		return cache.PackageData{}, err
	}
	return data, nil
}

func genericTemplateReferences(decl ast.Decl, info *check.ProgramInfo) []cache.GenericReference {
	if info == nil {
		return nil
	}
	program := ast.Program{Files: []ast.File{{Decls: []ast.Decl{decl}}}}
	var references []cache.GenericReference
	ast.WalkExpressions(&program, func(expr *ast.Expression) {
		if expr.Kind != ast.ExprIdent {
			return
		}
		object, ok := info.Object(info.Uses[expr.NodeID])
		if ok && object.Kind == check.ObjectImport {
			references = append(references, cache.GenericReference{Node: expr.NodeID, Name: object.Name, Span: expr.Span, Kind: uint8(object.Kind), ModulePath: object.ImportPath})
			return
		}
		if !ok || object.Scope != info.PackageScope {
			return
		}
		reference := cache.GenericReference{Node: expr.NodeID, Name: object.Name, Span: expr.Span, Kind: uint8(object.Kind), Type: types.FormatWithTable(info.TypeTable, object.Type), Untyped: object.Untyped, Generic: len(info.GenericDecls[object.ID]) != 0}
		if value, found := info.ConstObjects[object.ID]; found {
			reference.Value = value.ExactText()
			if value.Imag != "" {
				reference.Value, reference.Imaginary = value.Real, value.Imag
			}
		}
		references = append(references, reference)
	})
	ast.WalkTypes(&program, func(typ *ast.TypeExpr) {
		object, ok := info.Object(info.Uses[typ.NodeID])
		if ok && object.Kind == check.ObjectImport {
			references = append(references, cache.GenericReference{Node: typ.NodeID, Name: object.Name, Span: typ.Span, Kind: uint8(object.Kind), ModulePath: object.ImportPath})
			return
		}
		if !ok || object.Scope != info.PackageScope || len(info.GenericDecls[object.ID]) == 0 {
			return
		}
		references = append(references, cache.GenericReference{Node: typ.NodeID, Name: object.Name, Span: typ.Span, Kind: uint8(object.Kind), Generic: true})
	})
	return references
}

func genericReceiverName(typ ast.TypeExpr) string {
	if typ.Kind == ast.TypePointer && typ.Elem != nil {
		typ = *typ.Elem
	}
	if typ.Kind == ast.TypeInstance && typ.Base != nil {
		typ = *typ.Base
	}
	if typ.Kind != ast.TypeName {
		return ""
	}
	name := typ.Name
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = name[dot+1:]
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func isExported(name string) bool {
	return token.IsExportedName(strings.TrimSpace(name))
}
