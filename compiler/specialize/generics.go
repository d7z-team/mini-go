// Package specialize instantiates checked generic declarations for concrete uses.
package specialize

import (
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/cache"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
)

type genericDecl struct {
	decl             ast.Decl
	file             string
	typeParams       []ast.TypeParam
	receiverBindings map[string]ast.TypeExpr
	imports          []ast.ImportDecl
	importAlias      string
	namedTypes       map[string]ast.TypeExpr
}

type genericMethod struct {
	decl    ast.FuncDecl
	pointer bool
}

type genericTypeInstance struct {
	base string
	args []ast.TypeExpr
}

type specializedDecl struct {
	decl    ast.Decl
	file    string
	imports []ast.ImportDecl
}

type genericSpecializer struct {
	info             *check.ProgramInfo
	functions        map[string]genericDecl
	types            map[string]genericDecl
	methods          map[string][]genericDecl
	generated        map[string]string
	activeDecl       map[string]string
	output           []specializedDecl
	diagnostics      *source.DiagnosticCollector
	maxInstances     int
	instances        int
	importPaths      map[string]string
	importedFiles    map[string]ast.File
	typeDefs         map[string]ast.TypeExpr
	methodSets       map[string]map[string]genericMethod
	generatedFunc    map[string]ast.FuncDecl
	typeInstances    map[string]genericTypeInstance
	valueTypes       map[check.ObjectID]ast.TypeExpr
	activeAlias      string
	activeNamedTypes map[string]ast.TypeExpr
	resultTypes      []ast.TypeExpr
}

// Apply removes generic declarations by validating and materializing every
// source-level instantiation before ordinary semantic checking and emit.
func Apply(checked check.CheckedProgram, dependencies map[string]cache.PackageData) (ast.Program, []source.Diagnostic, error) {
	return ApplyWithLimits(checked, dependencies, Limits{})
}

const (
	defaultMaxSpecializations = 100_000
	defaultMaxDiagnostics     = 100
)

type Limits struct {
	MaxSpecializations int
	MaxDiagnostics     int
}

// Required reports whether source or dependency templates require the generic
// rewriting pass. A false result permits reusing the checked syntax unchanged.
func Required(checked check.CheckedProgram, dependencies map[string]cache.PackageData) bool {
	if checked.Info == nil || len(checked.Info.GenericDecls) != 0 || len(checked.Info.Instances) != 0 {
		return true
	}
	// A transitive dependency may export templates that this package never
	// references. Their presence alone must not clone and re-check the AST.
	var templates map[string]map[string]bool
	for module, dependency := range dependencies {
		if len(dependency.GenericTemplates) == 0 {
			continue
		}
		if templates == nil {
			templates = make(map[string]map[string]bool)
		}
		names := make(map[string]bool, len(dependency.GenericTemplates))
		for _, template := range dependency.GenericTemplates {
			names[template.Name] = true
		}
		templates[module] = names
	}
	if len(templates) == 0 {
		return false
	}
	// Inference and function-value contexts can precede Instance facts. The
	// selected package member and dot-import object still identify the template.
	for _, selection := range checked.Info.Selections {
		if templates[selection.ModulePath][selection.Name] || selection.Object != "" && len(checked.Info.GenericDecls[selection.Object]) != 0 {
			return true
		}
	}
	for _, id := range checked.Info.Uses {
		object := checked.Info.Objects[id]
		if templates[object.ModulePath][object.ExportName] {
			return true
		}
	}
	// Explicit source types can contain an instance below a slice, field or
	// signature without a value-expression Instance entry.
	for _, info := range checked.Info.Types {
		if info.Type.Valid() && !checked.Info.TypeExact(info.Type) {
			return true
		}
	}
	return false
}

func ApplyWithLimits(checked check.CheckedProgram, dependencies map[string]cache.PackageData, limits Limits) (ast.Program, []source.Diagnostic, error) {
	if limits.MaxSpecializations <= 0 {
		limits.MaxSpecializations = defaultMaxSpecializations
	}
	if limits.MaxDiagnostics <= 0 {
		limits.MaxDiagnostics = defaultMaxDiagnostics
	}
	program := checked.Program
	info := checked.Info
	out := ast.CloneProgram(program)
	s := &genericSpecializer{
		info: info, functions: make(map[string]genericDecl), types: make(map[string]genericDecl),
		methods: make(map[string][]genericDecl), generated: make(map[string]string), activeDecl: make(map[string]string),
		importPaths: make(map[string]string), importedFiles: make(map[string]ast.File),
		typeDefs:      make(map[string]ast.TypeExpr),
		methodSets:    make(map[string]map[string]genericMethod),
		generatedFunc: make(map[string]ast.FuncDecl),
		typeInstances: make(map[string]genericTypeInstance),
		valueTypes:    make(map[check.ObjectID]ast.TypeExpr),
		diagnostics:   source.NewDiagnosticCollector(limits.MaxDiagnostics),
		maxInstances:  limits.MaxSpecializations,
	}
	s.bindSourceImports(&out)
	s.collectImportedGenerics(out, dependencies)
	localTypes := make(map[string]ast.TypeExpr)
	for i := range out.Files {
		for j := range out.Files[i].Decls {
			decl := out.Files[i].Decls[j]
			if decl.Kind != ast.DeclType || len(decl.Type.TypeParams) != 0 {
				continue
			}
			name := decl.Type.Name
			localTypes[name] = decl.Type.Type
			s.typeDefs[name] = decl.Type.Type
		}
	}
	for i := range out.Files {
		for j := range out.Files[i].Decls {
			decl := out.Files[i].Decls[j]
			if decl.Kind == ast.DeclFunc && decl.Func.Receiver != nil {
				s.registerMethod(genericReceiverName(decl.Func.Receiver.Type), *decl.Func, decl.Func.Receiver.Type.Kind == ast.TypePointer)
			}
		}
	}
	for i := range out.Files {
		for j := range out.Files[i].Decls {
			decl := out.Files[i].Decls[j]
			if decl.Kind == ast.DeclType && len(decl.Type.TypeParams) != 0 {
				s.types[decl.Type.Name] = genericDecl{decl: decl, typeParams: decl.Type.TypeParams, namedTypes: localTypes}
			}
			if decl.Kind == ast.DeclFunc && decl.Func.Receiver == nil && len(decl.Func.TypeParams) != 0 {
				s.functions[decl.Func.Name] = genericDecl{decl: decl, typeParams: decl.Func.TypeParams, namedTypes: localTypes}
			}
		}
	}
	for i := range out.Files {
		for j := range out.Files[i].Decls {
			decl := out.Files[i].Decls[j]
			if decl.Kind == ast.DeclFunc && decl.Func.Receiver != nil {
				if receiver := genericReceiverName(decl.Func.Receiver.Type); s.types[receiver].decl.Kind != "" || len(decl.Func.TypeParams) != 0 {
					s.methods[receiver] = append(s.methods[receiver], genericDecl{decl: decl, namedTypes: localTypes})
				}
			}
		}
	}
	for i := range out.Files {
		kept := make([]ast.Decl, 0, len(out.Files[i].Decls))
		for j := range out.Files[i].Decls {
			decl := out.Files[i].Decls[j]
			if decl.Kind == ast.DeclType && len(decl.Type.TypeParams) != 0 ||
				decl.Kind == ast.DeclFunc && (len(decl.Func.TypeParams) != 0 || decl.Func.Receiver != nil && s.types[genericReceiverName(decl.Func.Receiver.Type)].decl.Kind != "") {
				continue
			}
			s.rewriteDecl(&decl, nil)
			kept = append(kept, decl)
		}
		out.Files[i].Decls = kept
	}
	s.appendTemplateGlobalAccessors(&out, program)
	if len(out.Files) != 0 {
		for _, generated := range s.output {
			decl := generated.decl
			owner := 0
			for i := range out.Files {
				if out.Files[i].Path == generated.file || generated.file == "" && out.Files[i].Span.Start.File == decl.Span.Start.File {
					owner = i
					break
				}
			}
			out.Files[owner].Decls = append(out.Files[owner].Decls, decl)
			for _, imported := range generated.imports {
				found := false
				for _, existing := range out.Files[owner].Decls {
					found = found || existing.Kind == ast.DeclImport && existing.Import.Alias == imported.Alias && existing.Import.Path == imported.Path
				}
				if !found {
					copied := imported
					out.Files[owner].Decls = append(out.Files[owner].Decls, ast.Decl{Kind: ast.DeclImport, Import: &copied})
				}
			}
		}
	}
	fileKeys := make([]string, 0, len(s.importedFiles))
	for key := range s.importedFiles {
		fileKeys = append(fileKeys, key)
	}
	sort.Strings(fileKeys)
	for _, key := range fileKeys {
		file := s.importedFiles[key]
		found := false
		for i := range out.Files {
			if out.Files[i].Path == file.Path {
				found = true
				break
			}
		}
		if !found {
			out.Files = append(out.Files, file)
		}
	}
	s.retainImportInitializers(&out, checked.Program)
	return out, s.diagnostics.Diagnostics(), nil
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
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	return name
}

func (s *genericSpecializer) instantiateFunction(name string, args []ast.TypeExpr, span source.Span) string {
	generic, ok := s.functions[name]
	if !ok && s.activeAlias != "" {
		name = s.activeAlias + "." + name
		generic, ok = s.functions[name]
	}
	if !ok {
		return ""
	}
	args = cloneGenericTypes(args)
	for i := range args {
		s.canonicalizeTypeImports(&args[i])
	}
	if !s.validateTypeArgs(name, generic, args, span) {
		return ""
	}
	identityName := name
	for alias, path := range s.importPaths {
		if strings.HasPrefix(name, alias+".") {
			identityName = path + strings.TrimPrefix(name, alias)
			break
		}
	}
	key, generatedName := specializationNames("function", identityName, args)
	if existing := s.generated[key]; existing != "" {
		return existing
	}
	if activeKey := s.activeDecl["function:"+name]; activeKey != "" && activeKey != key {
		s.addDiagnostic("compiler.generic.cycle", "generic instantiation cycle expands type arguments for "+name, span)
		return ""
	}
	if !s.reserveSpecialization(span) {
		return ""
	}
	s.generated[key] = generatedName
	s.activeDecl["function:"+name] = key
	decl := cloneGenericDecl(generic.decl)
	substitutions := bindTypeArgs(generic.typeParams, args)
	for param, typ := range generic.receiverBindings {
		substitutions[param] = typ
	}
	decl.Func.Name = generatedName
	decl.Func.TypeParams = nil
	if generic.importAlias != "" {
		decl.Func.DefinitionModule = s.importPaths[generic.importAlias]
	}
	previousAlias := s.activeAlias
	previousNamedTypes := s.activeNamedTypes
	s.activeAlias = generic.importAlias
	s.activeNamedTypes = generic.namedTypes
	s.rewriteDecl(&decl, substitutions)
	s.activeAlias = previousAlias
	s.activeNamedTypes = previousNamedTypes
	s.generatedFunc[generatedName] = *decl.Func
	s.output = append(s.output, specializedDecl{decl: decl, file: generic.file, imports: generic.imports})
	delete(s.activeDecl, "function:"+name)
	return generatedName
}

func (s *genericSpecializer) instantiateType(name string, args []ast.TypeExpr, span source.Span) string {
	generic, ok := s.types[name]
	if !ok && s.activeAlias != "" {
		name = s.activeAlias + "." + name
		generic, ok = s.types[name]
	}
	if !ok {
		return ""
	}
	args = cloneGenericTypes(args)
	for i := range args {
		s.canonicalizeTypeImports(&args[i])
	}
	if !s.validateTypeArgs(name, generic, args, span) {
		return ""
	}
	identityName := name
	for alias, path := range s.importPaths {
		if strings.HasPrefix(name, alias+".") {
			identityName = path + strings.TrimPrefix(name, alias)
			break
		}
	}
	key, generatedName := specializationNames("type", identityName, args)
	if existing := s.generated[key]; existing != "" {
		return existing
	}
	if activeKey := s.activeDecl["type:"+name]; activeKey != "" && activeKey != key {
		s.addDiagnostic("compiler.generic.cycle", "generic instantiation cycle expands type arguments for "+name, span)
		return ""
	}
	if !s.reserveSpecialization(span) {
		return ""
	}
	s.generated[key] = generatedName
	s.typeInstances[generatedName] = genericTypeInstance{base: name, args: append([]ast.TypeExpr(nil), args...)}
	s.activeDecl["type:"+name] = key
	substitutions := bindTypeArgs(generic.typeParams, args)
	decl := cloneGenericDecl(generic.decl)
	decl.Type.Name = generatedName
	decl.Type.TypeParams = nil
	previousAlias := s.activeAlias
	previousNamedTypes := s.activeNamedTypes
	s.activeAlias = generic.importAlias
	s.activeNamedTypes = generic.namedTypes
	s.rewriteDecl(&decl, substitutions)
	s.typeDefs[generatedName] = decl.Type.Type
	s.output = append(s.output, specializedDecl{decl: decl, file: generic.file, imports: generic.imports})
	for _, methodTemplate := range s.methods[name] {
		if len(methodTemplate.decl.Func.TypeParams) != 0 {
			continue
		}
		method := cloneGenericDecl(methodTemplate.decl)
		pointer := method.Func.Receiver != nil && method.Func.Receiver.Type.Kind == ast.TypePointer
		substituteReceiverTypeParams(method.Func.Receiver, generic.typeParams, args, substitutions)
		s.activeAlias = methodTemplate.importAlias
		s.activeNamedTypes = methodTemplate.namedTypes
		s.rewriteDecl(&method, substitutions)
		setSpecializedReceiver(method.Func.Receiver, generatedName)
		s.registerMethod(generatedName, *method.Func, pointer)
		s.output = append(s.output, specializedDecl{decl: method, file: methodTemplate.file, imports: methodTemplate.imports})
	}
	s.activeAlias = previousAlias
	s.activeNamedTypes = previousNamedTypes
	delete(s.activeDecl, "type:"+name)
	return generatedName
}

func (s *genericSpecializer) reserveSpecialization(span source.Span) bool {
	if s.instances >= s.maxInstances {
		s.addDiagnostic("compiler.generic.limit", "generic specialization count exceeds compiler limit", span)
		return false
	}
	s.instances++
	return true
}
