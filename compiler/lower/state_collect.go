package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) collectImports(program ast.Program) {
	seenPath := map[string]struct{}{}
	for _, file := range program.Files {
		fileKey := importFileKey(file)
		fileImports := l.importsByFile[fileKey]
		if fileImports == nil {
			fileImports = map[string]string{}
			l.importsByFile[fileKey] = fileImports
		}
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclImport {
				continue
			}
			if decl.Import.IsEmbedMarker() {
				continue
			}
			path := strings.TrimSpace(decl.Import.Path)
			if path == "" {
				continue
			}
			if _, ok := seenPath[path]; !ok {
				seenPath[path] = struct{}{}
				l.importPaths = append(l.importPaths, path)
			}
			alias := importAlias(*decl.Import)
			if alias == "_" || alias == "." || alias == "" {
				continue
			}
			fileImports[alias] = path
			if existingPath, exists := l.imports[alias]; !exists {
				l.imports[alias] = path
			} else if existingPath != path {
				delete(l.imports, alias)
			}
		}
	}
}

func importFileKey(file ast.File) string {
	if strings.TrimSpace(file.Path) != "" {
		return strings.TrimSpace(file.Path)
	}
	return strings.TrimSpace(file.ID)
}

func (l *lowerer) collectDotImports(program ast.Program) {
	for _, file := range program.Files {
		fileKey := importFileKey(file)
		fileDotImports := l.dotImportsByFile[fileKey]
		if fileDotImports == nil {
			fileDotImports = map[string]moduleExportInfo{}
			l.dotImportsByFile[fileKey] = fileDotImports
		}
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclImport || importAlias(*decl.Import) != "." {
				continue
			}
			modulePath := strings.TrimSpace(decl.Import.Path)
			for name, export := range l.moduleExports[modulePath] {
				if !isExported(name) {
					continue
				}
				fileDotImports[name] = export
				l.recordUnambiguousDotImport(name, export)
			}
		}
	}
}

func (l *lowerer) recordUnambiguousDotImport(name string, export moduleExportInfo) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if previous, exists := l.unambiguousDotImports[name]; exists {
		if previous.ModulePath != export.ModulePath {
			delete(l.unambiguousDotImports, name)
		}
		return
	}
	l.unambiguousDotImports[name] = export
}

func (l *lowerer) collectDependencyExports(exports []check.DependencyExport) {
	if l.importedTypes == nil {
		l.importedTypes = map[string]moduleExportInfo{}
	}
	for _, export := range exports {
		modulePath := strings.TrimSpace(export.ModulePath)
		name := strings.TrimSpace(export.Name)
		if modulePath == "" || name == "" {
			continue
		}
		if export.Kind != check.ObjectConst && export.Kind != check.ObjectVar && export.Kind != check.ObjectType && export.Kind != check.ObjectFunc {
			l.add("hirgen.dependency.kind", "dependency export has invalid kind", source.Span{})
			continue
		}
		info := moduleExportInfo{
			ModulePath: modulePath,
			Kind:       export.Kind,
			Type:       strings.TrimSpace(export.Type),
			Underlying: strings.TrimSpace(export.Underlying),
			Fields:     append([]check.DependencyTypeField(nil), export.Fields...),
			Variadic:   export.Variadic,
			Untyped:    export.Untyped,
		}
		if info.Kind == check.ObjectConst && len(export.Value) != 0 {
			ref, err := l.typeParser.Parse(info.Type)
			if err != nil {
				l.add("hirgen.dependency.constant", "invalid imported constant type", source.Span{})
				continue
			}
			kind := types.FormatWithTable(l.typeTable, l.typeTable.Underlying(ref))
			value, ok := constant.FromJSON(export.Value, kind, info.Untyped)
			if !ok {
				l.add("hirgen.dependency.constant", "invalid imported constant value", source.Span{})
				continue
			}
			info.Value = value.Ref()
		}
		for _, method := range export.Methods {
			if len(method.TypeParams) == 0 {
				info.Methods = append(info.Methods, method)
			}
		}
		if l.moduleExports[modulePath] == nil {
			l.moduleExports[modulePath] = map[string]moduleExportInfo{}
		}
		l.moduleExports[modulePath][name] = info
		if info.Kind == check.ObjectType {
			identity := modulePath + "." + name
			l.importedTypes[identity] = info
			if info.Type != "" {
				l.importedTypes[info.Type] = info
			}
		}
		l.registerDependencyTypeCatalog(name, info)
	}
}

func (l *lowerer) registerDependencyTypeCatalog(name string, info moduleExportInfo) {
	if info.Kind != check.ObjectType {
		return
	}
	modulePath := strings.TrimSpace(info.ModulePath)
	name = strings.TrimSpace(name)
	if modulePath == "" || name == "" {
		return
	}
	l.initTypeRefs()
	key := types.TypeKey{ModulePath: modulePath, DeclID: types.DeclID(name)}
	node, exists := l.typeTable.Named(key)
	if !exists {
		node = types.TypeNode{
			ID:       types.TypeID("decl." + modulePath + "." + name),
			Kind:     types.Named,
			Identity: key,
		}
	}
	self := modulePath + "." + name
	if target := strings.TrimSpace(info.Underlying); target != "" {
		ref, ok := l.parseDependencyTypeRef(modulePath, target)
		if !ok {
			return
		}
		node.Underlying = ref
	} else if target := strings.TrimSpace(info.Type); target != "" && target != self {
		ref, ok := l.parseDependencyTypeRef(modulePath, target)
		if !ok {
			return
		}
		node.Alias = true
		node.AliasTarget = ref
	} else {
		return
	}
	// Runtime bindings are supplied by compiled dependencies. Source analysis
	// deliberately has no executable method IDs; update our owned table without
	// changing the published semantic facts.
	node.Fields = nil
	node.Methods = nil
	for _, field := range info.Fields {
		ref, ok := l.parseDependencyTypeRef(modulePath, field.Type)
		if !ok {
			return
		}
		node.Fields = append(node.Fields, types.Field{
			Name:     strings.TrimSpace(field.Name),
			Type:     ref,
			Tag:      field.Tag,
			Embedded: field.Embedded,
		})
	}
	for _, method := range info.Methods {
		signature, ok := l.parseDependencyFunctionSignature(modulePath, method.Signature)
		if !ok {
			return
		}
		receiver, _ := l.parseDependencyTypeRef(modulePath, method.Receiver)
		node.Methods = append(node.Methods, types.Method{
			Name:       strings.TrimSpace(method.Name),
			Receiver:   receiver,
			Signature:  signature,
			FunctionID: strings.TrimSpace(method.FunctionID),
			ModulePath: strings.TrimSpace(firstNonEmpty(method.ModulePath, info.ModulePath)),
		})
	}
	if exists {
		_ = l.typeTable.Replace(node)
	} else {
		_ = l.typeTable.Add(node)
	}
}

func (l *lowerer) parseDependencyTypeRef(modulePath, text string) (types.TypeRef, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return types.TypeRef{}, false
	}
	l.initTypeRefs()
	parser := types.NewParser(strings.TrimSpace(firstNonEmpty(modulePath, l.modulePath)), l.typeTable)
	ref, err := parser.Parse(text)
	if err != nil || !ref.Valid() {
		return types.TypeRef{}, false
	}
	return ref, true
}

func (l *lowerer) parseDependencyFunctionSignature(modulePath, text string) (types.FunctionSignature, bool) {
	ref, ok := l.parseDependencyTypeRef(modulePath, text)
	if !ok || ref.Kind != types.Function || ref.Node == "" {
		return types.FunctionSignature{}, false
	}
	node, ok := l.typeTable.Node(ref)
	if !ok || node.Signature == nil {
		return types.FunctionSignature{}, false
	}
	return *node.Signature, true
}

func (l *lowerer) collectTopLevelSymbols(program ast.Program) {
	seen := map[string]source.Span{}
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			switch decl.Kind {
			case ast.DeclConst:
				for _, name := range decl.Const.Names {
					name = strings.TrimSpace(name)
					if isBlankIdentifier(name) {
						continue
					}
					if !l.registerPackageIdentifier(seen, name, decl.Span) {
						continue
					}
					l.packageSymbols[name] = struct{}{}
					l.constants[name] = constID(name)
				}
			case ast.DeclVar:
				declType := l.resolveSourceType(decl.Var.Type)
				for _, name := range decl.Var.Names {
					name = strings.TrimSpace(name)
					if isBlankIdentifier(name) {
						continue
					}
					if !l.registerPackageIdentifier(seen, name, decl.Span) {
						continue
					}
					l.packageSymbols[name] = struct{}{}
					l.globals[name] = globalID(name)
					if declType != "" {
						l.globalTypes[name] = l.hirType(declType)
					}
				}
			case ast.DeclType:
				name := strings.TrimSpace(decl.Type.Name)
				if isBlankIdentifier(name) {
					continue
				}
				if l.registerPackageIdentifier(seen, name, decl.Span) {
					l.packageSymbols[name] = struct{}{}
					if decl.Type.Alias {
						if target := l.resolveSourceType(decl.Type.Type); target != "" {
							l.typeAliases[name] = target
						}
						continue
					}
					l.typeDecls[name] = decl.Type.Type
				}
			case ast.DeclFunc:
				name := strings.TrimSpace(decl.Func.Name)
				paramTypes, variadic := l.paramTypes(decl.Func.Params)
				if decl.Func.Receiver != nil {
					if isBlankIdentifier(name) {
						continue
					}
					receiver := l.methodReceiverType(decl.Func.Receiver.Type)
					methodKey := receiver + "." + name
					if !l.registerSymbol(seen, methodKey, decl.Span) {
						continue
					}
					modulePath := ""
					if !isExported(name) {
						modulePath = l.currentModulePath()
					}
					l.methods[methodKey] = methodInfo{
						ModulePath: modulePath,
						FunctionID: methodID(receiver, name),
						Receiver:   l.hirType(receiver),
						Signature:  l.hirSignature(signatureFromTypes(paramTypes, l.resultTypes(decl.Func.Results)), variadic),
					}
				} else if name == "init" {
					if len(decl.Func.Params) != 0 || len(decl.Func.Results) != 0 || variadic {
						l.add("hirgen.init.signature", "init function must have no parameters or results", decl.Span)
					}
					continue
				} else if name == "main" && strings.TrimSpace(program.Package) == "main" {
					if len(decl.Func.Params) != 0 || len(decl.Func.Results) != 0 || variadic {
						l.add("hirgen.main.signature", "main function must have no parameters or results", decl.Span)
					}
					if !l.registerSymbol(seen, name, decl.Span) {
						continue
					}
				} else if isBlankIdentifier(name) {
					continue
				} else if !l.registerSymbol(seen, name, decl.Span) {
					continue
				}
				l.packageSymbols[name] = struct{}{}
				if decl.Func.Receiver == nil {
					l.functions[name] = functionID(name)
				}
			}
		}
	}
}
