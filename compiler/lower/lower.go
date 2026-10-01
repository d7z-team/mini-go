// Package lower converts checked Mini-Go syntax into typed HIR.
package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

type Options struct {
	Dependencies []check.DependencyPackage
}

func Lower(checked check.CheckedProgram, options Options) (ir.Program, []source.Diagnostic) {
	program := checked.Program
	if checked.Info == nil {
		return ir.Program{}, []source.Diagnostic{{Code: "compiler.semantic.missing", Severity: "error", Message: "missing checked semantic program"}}
	}
	semanticDiagnostics := checked.Info.Diagnostics
	if len(semanticDiagnostics) != 0 {
		return ir.Program{}, semanticDiagnostics
	}
	typeTable := &types.TypeTable{Nodes: append([]types.TypeNode(nil), checked.Info.TypeTable.Nodes...)}
	if err := typeTable.Reindex(); err != nil {
		return ir.Program{}, []source.Diagnostic{{Code: "compiler.semantic.type_table", Severity: "error", Message: err.Error()}}
	}
	l := lowerer{
		modulePath:            strings.TrimSpace(program.ModulePath),
		functions:             map[string]string{},
		semantic:              checked.Info,
		methods:               map[string]methodInfo{},
		typeDecls:             map[string]ast.TypeExpr{},
		typeAliases:           map[string]string{},
		constants:             map[string]string{},
		constValues:           map[string]constantValue{},
		globals:               map[string]string{},
		globalTypes:           map[string]types.TypeRef{},
		globalVariadics:       map[string]bool{},
		packageSymbols:        map[string]struct{}{},
		imports:               map[string]string{},
		importsByFile:         map[string]map[string]string{},
		unambiguousDotImports: map[string]moduleExportInfo{},
		dotImportsByFile:      map[string]map[string]moduleExportInfo{},
		implicitImports:       map[string]struct{}{},
		importExports:         map[string]map[string]struct{}{},
		moduleExports:         map[string]map[string]moduleExportInfo{},
		importedTypes:         map[string]moduleExportInfo{},
		labeledBranches:       map[string][]branchTarget{},
		typeTable:             typeTable,
		typeRefs:              map[string]types.TypeRef{},
	}
	l.typeParser = types.NewParser(l.modulePath, l.typeTable)
	l.relations = types.NewRelations(l.typeTable)
	l.collectImports(program)
	for _, dependency := range options.Dependencies {
		members := append([]check.DependencyExport(nil), dependency.Members...)
		for i := range members {
			members[i].ModulePath = dependency.ModulePath
		}
		l.collectDependencyExports(members)
	}
	l.collectDotImports(program)
	l.collectTopLevelSymbols(program)
	// Checked programs may belong to a published analysis snapshot. Local
	// type rewriting is the only syntax mutation performed by lowering.
	// Keep ordinary packages shared and copy syntax before that mutation.
	for _, object := range checked.Info.Objects {
		if object.Kind == check.ObjectType && object.Node != 0 && object.Scope != checked.Info.PackageScope {
			program = ast.CloneProgram(program)
			l.rewriteLocalTypes(&program)
			break
		}
	}
	l.semanticTypeText = map[types.TypeRef]string{}
	l.resolvedTypes = map[string]string{}
	l.namedUnderlyingTypes = map[string]string{}
	if len(l.diagnostics) != 0 {
		return ir.Program{}, l.diagnostics
	}
	out := ir.Program{
		ModulePath: program.ModulePath,
		Package:    program.Package,
	}
	l.program = &out
	out.DebugFiles = lowerDebugFiles(program.Files)
	out.DebugSourceHash = lowerDebugSourceHash(program.Files)
	out.Exports = append(out.Exports, l.lowerDefinedTypeExports(program)...)
	out.Exports = append(out.Exports, l.lowerTypeAliasExports(program)...)
	initFn := ir.Function{
		ID:            moduleInitFunctionID,
		Name:          "init",
		RevisionLocal: true,
		Generated:     true,
		Signature:     l.hirSignature("function() Void", false),
	}
	initScope := newFuncScope(&initFn, nil)
	initStmts, ok := l.lowerTopLevelValues(program, &out, &initScope)
	if !ok {
		if len(l.diagnostics) == 0 {
			l.add("hirgen.top_level.failed", "failed to lower package-level declarations", source.Span{})
		}
		return ir.Program{}, l.diagnostics
	}
	userInitIDs := make([]string, 0)
	blankFunctionIndex := 0
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclFunc {
				continue
			}
			name := strings.TrimSpace(decl.Func.Name)
			overrideID := ""
			userInit := false
			if decl.Func.Receiver == nil && name == "init" {
				overrideID = userInitFunctionID(len(userInitIDs))
				userInit = true
			} else if isBlankIdentifier(name) {
				overrideID = blankFunctionID(blankFunctionIndex)
				blankFunctionIndex++
			}
			fn, ok := l.lowerFuncDecl(decl, overrideID)
			if !ok {
				if len(l.diagnostics) == 0 {
					l.add("hirgen.function.failed", "failed to lower function "+name, decl.Span)
				}
				return ir.Program{}, l.diagnostics
			}
			out.Functions = append(out.Functions, fn)
			if userInit {
				userInitIDs = append(userInitIDs, overrideID)
			}
			if decl.Func.Receiver == nil && isExported(fn.Name) {
				out.Exports = append(out.Exports, ir.Export{
					Name: fn.Name,
					Kind: "function",
					ID:   fn.ID,
					Type: l.hirType(l.sourceFunctionTypeString(decl.Func.Params, decl.Func.Results)),
				})
			}
		}
	}
	out.Requirements = l.sourceRequirements()
	if len(out.Requirements) != 0 || len(initStmts) != 0 || len(userInitIDs) != 0 {
		for _, requirement := range out.Requirements {
			initFn.Body = append(initFn.Body, ir.Statement{Kind: ir.StmtInitModule, Module: requirement.ModulePath})
		}
		initFn.Body = append(initFn.Body, initStmts...)
		for _, initID := range userInitIDs {
			initFn.Body = append(initFn.Body, ir.Statement{
				Kind: ir.StmtExpr,
				Expr: ir.Expression{
					Kind:        ir.ExprCallDirect,
					Function:    initID,
					Type:        l.hirType("Void"),
					ResultCount: 0,
				},
			})
		}
		out.Functions = append(out.Functions, initFn)
	}
	out.Functions = append(out.Functions, l.extraFunctions...)
	if len(l.diagnostics) != 0 {
		return ir.Program{}, l.diagnostics
	}
	if err := l.bindRuntimeTypeMethods(program); err != nil {
		return ir.Program{}, []source.Diagnostic{{Code: "compiler.hir.type", Severity: "error", Message: err.Error()}}
	}
	if err := l.finalizeHIRTypeTable(&out); err != nil {
		return ir.Program{}, []source.Diagnostic{{Code: "compiler.hir.type", Severity: "error", Message: err.Error()}}
	}
	return out, nil
}

const moduleInitFunctionID = "fn.init"

func (l *lowerer) lowerDefinedTypeExports(program ast.Program) []ir.Export {
	var out []ir.Export
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclType || decl.Type.Alias {
				continue
			}
			name := strings.TrimSpace(decl.Type.Name)
			if name == "" || !isExported(name) {
				continue
			}
			out = append(out, ir.Export{Name: name, Kind: "type", ID: typeID(name), Type: l.hirType(name)})
		}
	}
	return out
}

func astFunctionTypeVariadic(typ ast.TypeExpr) bool {
	if typ.Kind != ast.TypeFunc || len(typ.Params) == 0 {
		return false
	}
	return typ.Params[len(typ.Params)-1].Variadic
}

const canonicalVariadicFunctionParam = "variadic "

func (l *lowerer) lowerTypeAliasExports(program ast.Program) []ir.Export {
	var out []ir.Export
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclType || !decl.Type.Alias {
				continue
			}
			name := strings.TrimSpace(decl.Type.Name)
			if name == "" || !isExported(name) {
				continue
			}
			target := l.resolveSourceType(decl.Type.Type)
			if target == "" {
				continue
			}
			out = append(out, ir.Export{
				Name: name,
				Kind: "type",
				ID:   typeID(name),
				Type: l.hirType(target),
			})
		}
	}
	return out
}
