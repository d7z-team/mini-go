package specialize

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/cache"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
	"github.com/d7z-team/mini-go/compiler/types"
)

// bindSourceImports makes imported references unambiguous before declarations are
// copied for specialization. The semantic binding, not the spelling, identifies
// each reference and preserves local shadowing and definition-site scope.
func (s *genericSpecializer) bindSourceImports(program *ast.Program) {
	aliases := make(map[string]string)
	used := make(map[string]bool, len(s.info.Objects))
	for _, object := range s.info.Objects {
		used[object.Name] = true
	}
	for i := range program.Files {
		for j := range program.Files[i].Decls {
			decl := &program.Files[i].Decls[j]
			if decl.Kind != ast.DeclImport || decl.Import.Alias == "_" || decl.Import.IsEmbedMarker() {
				continue
			}
			path := decl.Import.Path
			alias := aliases[path]
			if alias == "" {
				for n := len(aliases); ; n++ {
					alias = "_generic_import_" + strconv.Itoa(n)
					if !used[alias] {
						break
					}
				}
				used[alias] = true
				aliases[path] = alias
			}
			decl.Import.Alias = alias
		}
	}
	if len(aliases) == 0 {
		return
	}
	ast.WalkExpressions(program, func(expr *ast.Expression) {
		if expr.Kind != ast.ExprIdent {
			return
		}
		object, ok := s.info.Object(s.info.Uses[expr.NodeID])
		if ok && object.Kind == check.ObjectImport && aliases[object.ImportPath] != "" {
			expr.Name = aliases[object.ImportPath]
			return
		}
		if !ok || object.ExportName == "" || aliases[object.ModulePath] == "" {
			return
		}
		operand := ast.Expression{Kind: ast.ExprIdent, Name: aliases[object.ModulePath], Span: expr.Span}
		*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprSelector, Operand: &operand, Field: object.ExportName, Span: expr.Span}
	})
	ast.WalkTypes(program, func(typ *ast.TypeExpr) {
		if typ.Kind != ast.TypeName {
			return
		}
		object, ok := s.info.Object(s.info.Uses[typ.NodeID])
		if ok && object.Kind == check.ObjectImport && aliases[object.ImportPath] != "" {
			if dot := strings.IndexByte(typ.Name, '.'); dot >= 0 {
				typ.Name = aliases[object.ImportPath] + typ.Name[dot:]
			}
		}
		if ok && object.Kind == check.ObjectType && object.ExportName != "" && aliases[object.ModulePath] != "" {
			typ.Name = aliases[object.ModulePath] + "." + object.ExportName
		}
	})
}

func (s *genericSpecializer) collectImportedGenerics(program ast.Program, dependencies map[string]cache.PackageData) {
	collected := make(map[string]bool)
	collect := func(alias string, data cache.PackageData, file string) {
		collected[data.ModulePath] = true
		if alias == "" {
			alias = strings.TrimSpace(data.Package)
		}
		s.importPaths[alias] = data.ModulePath
		namedTypes := importedNamedTypes(data)
		for name, underlying := range namedTypes {
			s.typeDefs[alias+"."+name] = underlying
		}
		for _, typeDecl := range data.TypeTable.DefinedNamed(data.ModulePath) {
			receiver := alias + "." + string(typeDecl.Identity.DeclID)
			for _, method := range typeDecl.Methods {
				decl := ast.FuncDecl{Name: method.Name}
				for _, param := range method.Signature.Params {
					decl.Params = append(decl.Params, ast.Field{Type: importedTypeExpr(&data.TypeTable, param.Type)})
				}
				for _, result := range method.Signature.Results {
					decl.Results = append(decl.Results, ast.Field{Type: importedTypeExpr(&data.TypeTable, result)})
				}
				if method.Signature.Variadic && len(decl.Params) != 0 {
					decl.Params[len(decl.Params)-1].Variadic = true
				}
				s.registerMethod(receiver, decl, types.View(&data.TypeTable, method.Receiver).Shape() == types.Pointer)
			}
		}
		templateFiles := make(map[string]struct{}, len(data.GenericTemplates))
		for _, template := range data.GenericTemplates {
			decl := cloneGenericDecl(template.Decl)
			var templateImports []ast.ImportDecl
			if file == "" {
				templateImports = append(templateImports, ast.ImportDecl{Path: data.ModulePath, Alias: alias})
			}
			importAliases := make(map[string]string)
			for _, reference := range template.References {
				if check.ObjectKind(reference.Kind) != check.ObjectImport || importAliases[reference.Name] != "" {
					continue
				}
				_, importedAlias := specializationNames("import", reference.ModulePath, nil)
				importAliases[reference.Name] = importedAlias
				templateImports = append(templateImports, ast.ImportDecl{Path: reference.ModulePath, Alias: importedAlias})
				s.importPaths[importedAlias] = reference.ModulePath
				if imported, found := dependencies[reference.ModulePath]; found {
					for name, underlying := range importedNamedTypes(imported) {
						s.typeDefs[importedAlias+"."+name] = underlying
					}
				}
			}
			// Bind private references while original node IDs and source paths
			// still identify the checked declaration. No offset-only fallback is
			// needed after binding, including across template source files.
			referenceByNode := make(map[ast.NodeID]cache.GenericReference, len(template.References))
			for _, reference := range template.References {
				referenceByNode[reference.Node] = reference
			}
			bound := ast.Program{Files: []ast.File{{Decls: []ast.Decl{decl}}}}
			expressionTypes := make(map[ast.NodeID]types.TypeRef, len(template.Expressions))
			for _, expression := range template.Expressions {
				expressionTypes[expression.Node] = expression.Type
			}
			ast.WalkExpressions(&bound, func(expr *ast.Expression) {
				if ref, found := expressionTypes[expr.NodeID]; found && expr.Type == nil {
					typ := semanticTypeExpr(&data.TypeTable, ref, expr.Span)
					s.normalizeSourceTypeNames(&typ)
					expr.Type = &typ
				}
				reference, found := referenceByNode[expr.NodeID]
				if !found || expr.Kind != ast.ExprIdent || reference.Name != expr.Name || reference.Span.Start != expr.Span.Start {
					return
				}
				if check.ObjectKind(reference.Kind) == check.ObjectImport {
					expr.Name = importAliases[reference.Name]
					return
				}
				if reference.Generic || check.ObjectKind(reference.Kind) == check.ObjectType || check.ObjectKind(reference.Kind) == check.ObjectVar && token.IsExportedName(reference.Name) {
					operand := ast.Expression{Kind: ast.ExprIdent, Name: alias, Span: expr.Span}
					*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprSelector, Operand: &operand, Field: reference.Name, Span: expr.Span}
					return
				}
				parser := types.NewParser(data.ModulePath, &data.TypeTable)
				ref, err := parser.Parse(reference.Type)
				if err != nil {
					return
				}
				typ := semanticTypeExpr(&data.TypeTable, ref, expr.Span)
				s.normalizeSourceTypeNames(&typ)
				switch check.ObjectKind(reference.Kind) {
				case check.ObjectFunc:
					expr.FunctionModule, expr.FunctionID, expr.Type = data.ModulePath, "fn."+reference.Name, &typ
				case check.ObjectConst:
					literalType := semanticTypeExpr(&data.TypeTable, types.View(&data.TypeTable, ref).Underlying(), expr.Span)
					value := ast.Expression{Kind: ast.ExprLiteral, Literal: reference.Value, Type: &literalType, Span: expr.Span}
					if reference.Imaginary != "" {
						realType := ast.TypeExpr{Kind: ast.TypeName, Name: "Float64", Span: expr.Span}
						value.Type = &realType
						imaginary := ast.Expression{Kind: ast.ExprLiteral, Literal: reference.Imaginary + "i", Type: &literalType, Span: expr.Span}
						realPart := value
						value = ast.Expression{Kind: ast.ExprBinary, Left: &realPart, Right: &imaginary, Operator: "+", Span: expr.Span}
					}
					if !reference.Untyped {
						operand := value
						value = ast.Expression{Kind: ast.ExprConvert, Operand: &operand, Type: &typ, Span: expr.Span}
					}
					value.NodeID = expr.NodeID
					*expr = value
				case check.ObjectVar:
					if token.IsExportedName(reference.Name) {
						return
					}
					pointer := ast.TypeExpr{Kind: ast.TypePointer, Elem: &typ, Span: expr.Span}
					signature := ast.TypeExpr{Kind: ast.TypeFunc, Results: []ast.Field{{Type: pointer}}, Span: expr.Span}
					callee := ast.Expression{Kind: ast.ExprIdent, Name: "_generic_global_address_" + reference.Name, FunctionModule: data.ModulePath, FunctionID: "fn._generic_global_address_" + reference.Name, Type: &signature, Span: expr.Span}
					call := ast.Expression{Kind: ast.ExprCall, Callee: &callee, Type: &pointer, Span: expr.Span}
					*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprDeref, Operand: &call, Type: &typ, Span: expr.Span}
				}
			})
			ast.WalkTypes(&bound, func(typ *ast.TypeExpr) {
				reference, found := referenceByNode[typ.NodeID]
				if !found || check.ObjectKind(reference.Kind) != check.ObjectImport || typ.Kind != ast.TypeName || typ.Span.Start != reference.Span.Start {
					return
				}
				typ.Name = strings.Replace(typ.Name, reference.Name+".", importAliases[reference.Name]+".", 1)
			})
			ast.WalkExpressions(&bound, func(expr *ast.Expression) { expr.NodeID = 0 })
			if path := strings.TrimSpace(decl.Span.Start.File); path != "" {
				templateFiles[path] = struct{}{}
			}
			ast.RewriteDeclSourcePaths(&decl, func(path string) string {
				return importedSourcePath(data.ModulePath, path)
			})
			generic := genericDecl{
				decl: decl, file: file, importAlias: alias, namedTypes: namedTypes,
				imports: templateImports,
			}
			switch template.Kind {
			case "function":
				generic.typeParams = decl.Func.TypeParams
				s.functions[alias+"."+template.Name] = generic
				s.functions[data.ModulePath+"."+template.Name] = generic
			case "type":
				generic.typeParams = decl.Type.TypeParams
				s.types[alias+"."+template.Name] = generic
				s.types[data.ModulePath+"."+template.Name] = generic
			case "method":
				parts := strings.SplitN(template.Name, ".", 2)
				if len(parts) != 2 {
					continue
				}
				owner := alias + "." + parts[0]
				s.methods[owner] = append(s.methods[owner], generic)
				canonicalOwner := data.ModulePath + "." + parts[0]
				if canonicalOwner != owner {
					s.methods[canonicalOwner] = append(s.methods[canonicalOwner], generic)
				}
			}
		}
		for _, source := range data.SourceFiles {
			if _, ok := templateFiles[strings.TrimSpace(source.ID)]; !ok {
				if _, ok = templateFiles[strings.TrimSpace(source.Path)]; !ok {
					continue
				}
			}
			path := importedSourcePath(data.ModulePath, source.Path)
			s.importedFiles[path] = ast.File{ID: path, Path: path, Hash: source.Hash}
		}
	}

	for i := range program.Files {
		for j := range program.Files[i].Decls {
			decl := program.Files[i].Decls[j]
			if decl.Kind != ast.DeclImport || decl.Import.Alias == "_" {
				continue
			}
			importPath := strings.TrimSpace(decl.Import.Path)
			data, ok := dependencies[importPath]
			if !ok {
				continue
			}
			collect(strings.TrimSpace(decl.Import.Alias), data, program.Files[i].Path)
		}
	}
	for modulePath, data := range dependencies {
		if !collected[modulePath] {
			_, alias := specializationNames("import", modulePath, nil)
			collect(alias, data, "")
		}
	}
	for alias, path := range s.importPaths {
		for _, template := range dependencies[path].GenericTemplates {
			switch template.Kind {
			case "function":
				if generic, found := s.functions[path+"."+template.Name]; found {
					s.functions[alias+"."+template.Name] = generic
				}
			case "type":
				if generic, found := s.types[path+"."+template.Name]; found {
					s.types[alias+"."+template.Name] = generic
				}
			case "method":
				parts := strings.SplitN(template.Name, ".", 2)
				if len(parts) == 2 {
					s.methods[alias+"."+parts[0]] = s.methods[path+"."+parts[0]]
				}
			}
		}
	}
}

func importedNamedTypes(data cache.PackageData) map[string]ast.TypeExpr {
	declarations := data.TypeTable.DefinedNamed(data.ModulePath)
	out := make(map[string]ast.TypeExpr, len(declarations))
	for _, decl := range declarations {
		out[string(decl.Identity.DeclID)] = importedTypeExpr(&data.TypeTable, decl.Underlying)
	}
	return out
}

func importedTypeExpr(table *types.TypeTable, ref types.TypeRef) ast.TypeExpr {
	if ref.Kind == types.Named {
		return ast.TypeExpr{Kind: ast.TypeName, Name: types.FormatWithTable(table, ref)}
	}
	view := types.View(table, ref)
	switch view.Shape() {
	case types.Slice:
		elemRef, _ := view.Elem()
		elem := importedTypeExpr(table, elemRef)
		return ast.TypeExpr{Kind: ast.TypeSlice, Elem: &elem}
	case types.Pointer:
		elemRef, _ := view.Elem()
		elem := importedTypeExpr(table, elemRef)
		return ast.TypeExpr{Kind: ast.TypePointer, Elem: &elem}
	case types.Map:
		keyRef, elemRef, _ := view.Map()
		key := importedTypeExpr(table, keyRef)
		elem := importedTypeExpr(table, elemRef)
		return ast.TypeExpr{Kind: ast.TypeMap, Key: &key, Elem: &elem}
	case types.Interface:
		node, ok := table.Node(view.Underlying())
		if !ok {
			return ast.TypeExpr{Kind: ast.TypeName, Name: types.FormatWithTable(table, ref)}
		}
		result := ast.TypeExpr{Kind: ast.TypeInterface}
		for _, term := range node.Terms {
			result.Terms = append(result.Terms, ast.TypeTerm{Type: importedTypeExpr(table, term.Type), Approx: term.Approx})
		}
		for _, method := range node.Methods {
			fn := ast.FuncDecl{Name: method.Name}
			for _, param := range method.Signature.Params {
				fn.Params = append(fn.Params, ast.Field{Type: importedTypeExpr(table, param.Type)})
			}
			for _, resultType := range method.Signature.Results {
				fn.Results = append(fn.Results, ast.Field{Type: importedTypeExpr(table, resultType)})
			}
			result.Methods = append(result.Methods, fn)
		}
		return result
	}
	text := types.FormatWithTable(table, ref)
	return ast.TypeExpr{Kind: ast.TypeName, Name: text}
}

func (s *genericSpecializer) retainImportInitializers(program *ast.Program, original ast.Program) {
	sourceFiles := make(map[ast.NodeID]string, len(original.Files))
	for _, file := range original.Files {
		sourceFiles[file.NodeID] = file.Span.Start.File
	}
	for i := range program.Files {
		file := &program.Files[i]
		used := make(map[string]bool)
		mark := func(node ast.NodeID, span source.Span, name string) {
			objectID := s.info.Uses[node]
			if objectID == "" && len(s.info.Defs[node]) != 0 {
				objectID = s.info.Defs[node][0]
			}
			object, known := s.info.Object(objectID)
			scope := s.info.Scope(s.info.NodeScopes[node])
			for scope != nil && scope.Kind != check.ScopeFile {
				scope = s.info.Scope(scope.Parent)
			}
			// Original bindings distinguish package references from shadowed
			// locals. Imported templates have different source paths and IDs.
			if known && object.Name == name && scope != nil && sourceFiles[scope.Node] == span.Start.File {
				binding := s.info.Scope(object.Scope)
				if binding != nil && binding.Kind != check.ScopeFile {
					return
				}
			}
			used[name] = true
		}
		fileProgram := ast.Program{Files: []ast.File{*file}}
		ast.WalkExpressions(&fileProgram, func(expr *ast.Expression) {
			if expr.Kind == ast.ExprSelector && expr.Operand != nil && expr.Operand.Kind == ast.ExprIdent {
				mark(expr.Operand.NodeID, expr.Operand.Span, expr.Operand.Name)
			} else if expr.Kind == ast.ExprIdent && s.info.Uses[expr.NodeID] != "" {
				mark(expr.NodeID, expr.Span, expr.Name)
			}
		})
		ast.WalkTypes(&fileProgram, func(typ *ast.TypeExpr) {
			if typ.Kind == ast.TypeName {
				name := typ.Name
				if dot := strings.IndexByte(name, '.'); dot >= 0 {
					name = name[:dot]
				}
				mark(typ.NodeID, typ.Span, name)
			}
		})
		for j := range file.Decls {
			decl := &file.Decls[j]
			if decl.Kind != ast.DeclImport || decl.Import.IsEmbedMarker() {
				continue
			}
			alias := decl.Import.Alias
			if alias == "_" {
				continue
			}
			if alias == "" {
				alias = decl.Import.Path
				if slash := strings.LastIndexByte(alias, '/'); slash >= 0 {
					alias = alias[slash+1:]
				}
			}
			// Source imports were checked before rewriting. Keep initialization
			// when all references in this file became local specializations.
			if !used[alias] {
				decl.Import.Alias = "_"
			}
		}
	}
}

func importedSourcePath(modulePath, path string) string {
	modulePath = strings.Trim(strings.TrimSpace(modulePath), "/")
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if modulePath == "" || strings.HasPrefix(path, "<") {
		return path
	}
	if path == modulePath || strings.HasPrefix(path, modulePath+"/") {
		return path
	}
	return modulePath + "/" + path
}
