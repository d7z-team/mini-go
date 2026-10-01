package semantic

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	bytecode "github.com/d7z-team/mini-go/runtime/bytecode"
)

type analyzer struct {
	info             *ProgramInfo
	nextScope        ScopeID
	nextObj          uint64
	parser           *types.Parser
	files            map[string]ScopeID
	dependencies     map[string]map[string]DependencyExport
	diagnostics      map[string]struct{}
	constantBindings map[ObjectID]constantBinding
	constantStates   map[ObjectID]uint8
	variableBindings map[ObjectID]*variableBinding
	resultTypes      []types.TypeRef
}

// Check validates source structure and computes all source-level semantic
// facts consumed by specialization and emit.
func Check(program ast.Program) CheckedProgram {
	return WithOptions(program, AnalyzeOptions{})
}

func WithOptions(program ast.Program, options AnalyzeOptions) CheckedProgram {
	structural, _ := ast.FinalizeStructure(&program, options.Limits)
	if source.HasErrors(structural) {
		return CheckedProgram{Program: program, Info: &ProgramInfo{ModulePath: program.ModulePath, Package: program.Package, Diagnostics: structural}}
	}
	info := analyzeWithOptions(program, options)
	info.Diagnostics = append(info.Diagnostics, ast.ValidateProgramContentsWithLimits(program, options.Limits)...)
	return CheckedProgram{Program: program, Info: info}
}

// WithParsed consumes an owned source package. Its immutable construction phase
// has already finalized structure and node identities after all source edits.
// The returned AST and documents are owned by the caller; future analysis of a
// modified AST must enter through WithOptions.
func WithParsed(input parser.Package, options AnalyzeOptions) (CheckedProgram, []parser.Document) {
	program, documents, diagnostics := input.Take(options.Limits)
	if source.HasErrors(diagnostics) {
		return CheckedProgram{Program: program, Info: &ProgramInfo{ModulePath: program.ModulePath, Package: program.Package, Diagnostics: diagnostics}}, documents
	}
	info := analyzeWithOptions(program, options)
	info.Diagnostics = append(info.Diagnostics, ast.ValidateProgramContentsWithLimits(program, options.Limits)...)
	return CheckedProgram{Program: program, Info: info}, documents
}

func analyzeWithOptions(program ast.Program, options AnalyzeOptions) *ProgramInfo {
	table := &types.TypeTable{}
	info := &ProgramInfo{
		ModulePath:     program.ModulePath,
		Package:        program.Package,
		TypeTable:      table,
		Relations:      types.NewRelations(table),
		Constants:      make(map[ast.NodeID]constant.Value),
		ConstObjects:   make(map[ObjectID]constant.Value),
		ArrayLengths:   make(map[ast.NodeID]int64),
		Scopes:         make(map[ScopeID]*Scope),
		NodeScopes:     make(map[ast.NodeID]ScopeID),
		Objects:        make(map[ObjectID]Object),
		Defs:           make(map[ast.NodeID][]ObjectID),
		Uses:           make(map[ast.NodeID]ObjectID),
		NameDefs:       make(map[ast.NameID]ObjectID),
		NameUses:       make(map[ast.NameID]ObjectID),
		Exprs:          make(map[ast.NodeID]ExprInfo),
		Types:          make(map[ast.NodeID]TypeInfo),
		Calls:          make(map[ast.NodeID]CallInfo),
		Instances:      make(map[ast.NodeID]InstanceInfo),
		Selections:     make(map[ast.NodeID]Selection),
		Operators:      make(map[ast.NodeID]OperatorSelection),
		Composites:     make(map[ast.NodeID]CompositeInfo),
		Embeds:         make(map[ast.NodeID]EmbedInfo),
		Switches:       make(map[ast.NodeID]SwitchInfo),
		IntrinsicCalls: make(map[ast.NodeID]bytecode.IntrinsicID),
		GenericDecls:   make(map[ObjectID][]ObjectID),
		Functions:      make(map[ast.NodeID]ObjectID),
	}
	a := &analyzer{
		info: info, parser: types.NewParser(program.ModulePath, table),
		files: make(map[string]ScopeID), dependencies: make(map[string]map[string]DependencyExport),
		diagnostics:      make(map[string]struct{}),
		constantBindings: make(map[ObjectID]constantBinding),
		constantStates:   make(map[ObjectID]uint8),
		variableBindings: make(map[ObjectID]*variableBinding),
	}
	var members []DependencyExport
	for _, dependency := range options.Dependencies {
		a.dependencies[dependency.ModulePath] = make(map[string]DependencyExport)
		for _, member := range dependency.Members {
			member.ModulePath = dependency.ModulePath
			members = append(members, member)
		}
	}
	a.registerDependencies(members)
	info.Universe = a.newScope(ScopeUniverse, 0, 0)
	a.declareUniverse()
	info.PackageScope = a.newScope(ScopePackage, program.NodeID, info.Universe)
	a.declarePackage(program)
	a.analyzeFiles(program)
	a.validateMethodDeclarations(program)
	a.validateTypeContracts(&program)
	a.validateResolvedIdentifiers(&program)
	a.validateImportUsage(program)
	return info
}

func (a *analyzer) newScope(kind ScopeKind, node ast.NodeID, parent ScopeID) ScopeID {
	a.nextScope++
	id := a.nextScope
	a.info.Scopes[id] = &Scope{ID: id, Kind: kind, Node: node, Parent: parent, Objects: make(map[string]ObjectID)}
	if node != 0 {
		a.info.NodeScopes[node] = id
	}
	return id
}

func (a *analyzer) addDiagnostic(code, message string, span source.Span) {
	key := fmt.Sprintf("%s:%s:%d:%d", code, span.Start.File, span.Start.Offset, span.End.Offset)
	if _, exists := a.diagnostics[key]; exists {
		return
	}
	a.diagnostics[key] = struct{}{}
	a.info.Diagnostics = append(a.info.Diagnostics, source.Diagnostic{
		ModulePath: a.info.ModulePath,
		Code:       source.DiagnosticCode(code), Severity: source.SeverityError, Message: message, Primary: span,
	})
}

func (a *analyzer) declare(scopeID ScopeID, kind ObjectKind, name string, node ast.NodeID, typ types.TypeRef, mutable, alias bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "_" {
		return
	}
	scope := a.info.Scopes[scopeID]
	if _, ok := scope.Objects[name]; ok {
		return
	}
	a.nextObj++
	id := ObjectID("object." + strconv.FormatUint(a.nextObj, 10) + "." + name)
	object := Object{
		ID: id, Kind: kind, Name: name, Node: node, Scope: scopeID, Type: typ,
		Exported: isExported(name), Mutable: mutable, Alias: alias,
	}
	a.info.Objects[id] = object
	scope.Objects[name] = id
	if node != 0 {
		a.info.Defs[node] = append(a.info.Defs[node], id)
	}
}

func (a *analyzer) declareUniverse() {
	primitives := []struct {
		name string
		typ  types.TypeRef
	}{
		{"any", types.AnyType()},
		{"bool", types.Builtin(types.PrimitiveBool)},
		{"string", types.Builtin(types.PrimitiveString)},
		{"int", types.Builtin(types.PrimitiveInt)},
		{"int8", types.Builtin(types.PrimitiveInt8)},
		{"int16", types.Builtin(types.PrimitiveInt16)},
		{"int32", types.Builtin(types.PrimitiveInt32)},
		{"int64", types.Builtin(types.PrimitiveInt64)},
		{"uint", types.Builtin(types.PrimitiveUint)},
		{"uint8", types.Builtin(types.PrimitiveUint8)},
		{"uint16", types.Builtin(types.PrimitiveUint16)},
		{"uint32", types.Builtin(types.PrimitiveUint32)},
		{"uint64", types.Builtin(types.PrimitiveUint64)},
		{"uintptr", types.Builtin(types.PrimitiveUintptr)},
		{"float32", types.Builtin(types.PrimitiveFloat32)},
		{"float64", types.Builtin(types.PrimitiveFloat64)},
		{"complex64", types.Builtin(types.PrimitiveComplex64)},
		{"complex128", types.Builtin(types.PrimitiveComplex128)},
		{"byte", types.Builtin(types.PrimitiveUint8)},
		{"rune", types.Builtin(types.PrimitiveInt32)},
	}
	for _, primitive := range primitives {
		a.declare(a.info.Universe, ObjectType, primitive.name, 0, primitive.typ, false, primitive.name == "byte" || primitive.name == "rune")
	}
	// Predeclared error is universe-owned; its method must not inherit the
	// currently compiled module as the defining module.
	errorType, _ := types.NewParser("", a.info.TypeTable).Parse("interface{Error:function() String}")
	a.declare(a.info.Universe, ObjectType, "error", 0, errorType, false, false)
	comparable := types.TypeRef{Kind: types.Interface, Node: "universe.comparable"}
	_ = a.info.TypeTable.Add(types.TypeNode{ID: comparable.Node, Kind: types.Interface, Name: "comparable", TypeSet: true})
	a.declare(a.info.Universe, ObjectType, "comparable", 0, comparable, false, false)
	for _, name := range predeclaredBuiltinNames {
		a.declare(a.info.Universe, ObjectBuiltin, name, 0, types.TypeRef{}, false, false)
	}
}

func (a *analyzer) declarePackage(program ast.Program) {
	for i := range program.Files {
		file := &program.Files[i]
		for j := range file.Decls {
			decl := &file.Decls[j]
			switch decl.Kind {
			case ast.DeclConst:
				for _, name := range decl.Const.Names {
					a.declare(a.info.PackageScope, ObjectConst, name, decl.NodeID, a.typeOf(decl.Const.Type), false, false)
				}
			case ast.DeclVar:
				for _, name := range decl.Var.Names {
					a.declare(a.info.PackageScope, ObjectVar, name, decl.NodeID, a.typeOf(decl.Var.Type), true, false)
				}
			case ast.DeclType:
				name := decl.Type.Name
				ref, err := a.parser.Define(name, types.AnyType(), decl.Type.Alias)
				if err != nil {
					ref = types.TypeRef{}
				}
				a.declare(a.info.PackageScope, ObjectType, name, decl.NodeID, ref, false, decl.Type.Alias)
			case ast.DeclFunc:
				if decl.Func.Receiver == nil && decl.Func.Name != "init" {
					a.declare(a.info.PackageScope, ObjectFunc, decl.Func.Name, decl.NodeID, types.TypeRef{}, false, false)
				}
			}
		}
	}
}

func (a *analyzer) analyzeFiles(program ast.Program) {
	// Build all file scopes and imported bindings before resolving package types.
	for i := range program.Files {
		file := &program.Files[i]
		fileScope := a.newScope(ScopeFile, file.NodeID, a.info.PackageScope)
		a.files[file.Path] = fileScope
		for j := range file.Decls {
			decl := &file.Decls[j]
			if decl.Kind != ast.DeclImport || decl.Import.Alias == "_" {
				continue
			}
			if decl.Import.Alias == "." {
				a.declareDotImport(fileScope, decl.Import.Path, decl.NodeID)
				continue
			}
			name := decl.Import.Alias
			if name == "" {
				path := strings.TrimSuffix(decl.Import.Path, "/")
				if index := strings.LastIndex(path, "/"); index >= 0 {
					path = path[index+1:]
				}
				name = path
			}
			a.declare(fileScope, ObjectImport, name, decl.NodeID, types.TypeRef{}, false, false)
			if id, ok := a.info.Scopes[fileScope].Objects[name]; ok {
				object := a.info.Objects[id]
				object.ImportPath = strings.TrimSpace(decl.Import.Path)
				a.info.Objects[id] = object
			}
		}
	}
	a.indexPackageConstants(program)
	a.indexPackageVariables(program)
	// Resolve all package types before method sets and function bodies. Package
	// declaration order must not affect receiver or promoted selector facts.
	for i := range program.Files {
		file := &program.Files[i]
		fileScope := a.files[file.Path]
		for j := range file.Decls {
			decl := &file.Decls[j]
			if decl.Kind == ast.DeclType {
				a.analyzeDecl(decl, fileScope, false)
			}
		}
	}
	if !a.validateTypeCycles(program) {
		return
	}
	a.resolvePackageTypeDependencies(program)
	a.validateEmbeddedFieldTypes(&program)
	for i := range program.Files {
		file := &program.Files[i]
		fileScope := a.files[file.Path]
		for j := range file.Decls {
			decl := &file.Decls[j]
			if decl.Kind != ast.DeclFunc {
				continue
			}
			if decl.Func.Receiver == nil && decl.Func.Name != "init" {
				a.predeclareFunctionSignature(decl.Func, fileScope)
			} else if decl.Func.Receiver != nil {
				a.predeclareMethodSignature(decl.Func, fileScope)
			}
		}
	}
	for _, kind := range []ast.DeclKind{ast.DeclConst, ast.DeclVar, ast.DeclFunc} {
		for i := range program.Files {
			file := &program.Files[i]
			fileScope := a.files[file.Path]
			for j := range file.Decls {
				if file.Decls[j].Kind == kind {
					a.analyzeDecl(&file.Decls[j], fileScope, false)
				}
			}
		}
	}
}

func (a *analyzer) validateEmbeddedFieldTypes(program *ast.Program) {
	ast.WalkTypes(program, func(typ *ast.TypeExpr) {
		if typ.Kind != ast.TypeStruct {
			return
		}
		for i := range typ.Fields {
			field := &typ.Fields[i]
			if strings.TrimSpace(field.Name) == "" && !a.validEmbeddedFieldType(field.Type, false) {
				a.addDiagnostic("semantic.struct.embed.invalid", "embedded field must be a named type or pointer to a named non-pointer type", field.Span)
			}
		}
	})
}

// resolvePackageTypeDependencies refreshes package type shapes after every
// declaration has an identity and an initial underlying type. This makes
// interface embedding and other named-type dependencies independent of file
// and declaration order.
func (a *analyzer) resolvePackageTypeDependencies(program ast.Program) {
	typeCount := 0
	for i := range program.Files {
		for j := range program.Files[i].Decls {
			if program.Files[i].Decls[j].Kind == ast.DeclType {
				typeCount++
			}
		}
	}
	for pass := 0; pass < typeCount; pass++ {
		for i := range program.Files {
			for j := range program.Files[i].Decls {
				decl := &program.Files[i].Decls[j]
				if decl.Kind != ast.DeclType {
					continue
				}
				object, ok := a.info.Lookup(a.info.PackageScope, decl.Type.Name)
				if !ok || object.Type.Node == "" {
					continue
				}
				underlying := a.resolvedType(decl.Type.Type)
				if decl.Type.Type.Kind != ast.TypeName && decl.Type.Type.Kind != ast.TypeInstance {
					underlying = a.compositeType(decl.Type.Type)
				}
				node, exists := a.info.TypeTable.Node(object.Type)
				if !exists || !underlying.Valid() {
					continue
				}
				node.Alias = decl.Type.Alias
				if decl.Type.Alias {
					node.AliasTarget, node.Underlying = underlying, types.TypeRef{}
				} else {
					node.Underlying, node.AliasTarget = underlying, types.TypeRef{}
				}
				_ = a.info.TypeTable.Replace(node)
			}
		}
	}
}

func (a *analyzer) declareDotImport(scope ScopeID, modulePath string, node ast.NodeID) {
	modulePath = strings.TrimSpace(modulePath)
	module := a.dependencies[modulePath]
	names := make([]string, 0, len(module))
	for name := range module {
		if isExported(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		export := module[name]
		a.declare(scope, export.Kind, name, node, a.dependencyType(export), export.Kind == ObjectVar, export.Kind == ObjectType && strings.TrimSpace(export.Underlying) == "")
		id := a.info.Scopes[scope].Objects[name]
		object := a.info.Objects[id]
		object.ModulePath = modulePath
		object.ExportName = name
		object.FunctionID = strings.TrimSpace(export.ID)
		object.Untyped = export.Kind == ObjectConst && export.Untyped
		a.info.Objects[id] = object
	}
}
