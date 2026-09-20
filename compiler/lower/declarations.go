package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) lowerTopLevelValues(program ast.Program, out *ir.Program, initScope *funcScope) ([]ir.Statement, bool) {
	packageScope := newFuncScope(nil, nil)
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclVar && !l.validateVarDeclaration(decl.Var, decl.Span, &packageScope) {
				return nil, false
			}
		}
	}
	if !l.lowerPackageConstDecls(program, out) {
		return nil, false
	}
	l.inferPackageGlobalTypes(program)
	groups := l.collectGlobalInitGroups(program)
	functionDeps := l.packageFunctionDependencies(program)
	for i := range groups {
		groups[i].Deps = l.globalInitDependencies(groups[i].Values, functionDeps)
	}
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			switch decl.Kind {
			case ast.DeclVar:
				ok := l.declareGlobalDecl(decl, out)
				if !ok {
					return nil, false
				}
			}
		}
	}
	initStmts, ok := l.lowerGlobalInitGroups(groups, initScope)
	if !ok {
		return nil, false
	}
	return initStmts, true
}

func (l *lowerer) validateVarDeclaration(decl ast.ValueDecl, span source.Span, scope *funcScope) bool {
	if len(decl.Values) == 0 {
		if l.resolveSourceType(decl.Type) != "" {
			return true
		}
		l.add("hirgen.var.type_or_init", "var declaration requires a type or initializer", span)
		return false
	}
	if len(decl.Values) != 1 && len(decl.Values) != len(decl.Names) {
		l.add("hirgen.var.shape", "var declaration initializer count must match names", span)
		return false
	}
	if len(decl.Values) > 1 {
		for _, value := range decl.Values {
			if l.expressionResultCount(value, scope) != 1 {
				l.add("hirgen.var.shape", "multi-valued expression must be the only var initializer", value.Span)
				return false
			}
		}
	}
	return true
}

type globalInitTarget struct {
	Name     string
	Blank    bool
	Type     string
	Variadic bool
}

type globalInitGroup struct {
	Order   int
	Decl    ast.Decl
	Targets []globalInitTarget
	Values  []ast.Expression
	Deps    map[string]struct{}
}

func (l *lowerer) collectGlobalInitGroups(program ast.Program) []globalInitGroup {
	groups := []globalInitGroup{}
	order := 0
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclVar {
				continue
			}
			targets := l.globalInitTargets(decl)
			values := append([]ast.Expression(nil), decl.Var.Values...)
			if len(values) == 0 {
				for _, target := range targets {
					groups = append(groups, globalInitGroup{
						Order:   order,
						Decl:    decl,
						Targets: []globalInitTarget{target},
					})
					order++
				}
				continue
			}
			if len(targets) > 1 && len(values) == 1 {
				groups = append(groups, globalInitGroup{Order: order, Decl: decl, Targets: targets, Values: values})
				order++
				continue
			}
			for i, target := range targets {
				group := globalInitGroup{Order: order, Decl: decl, Targets: []globalInitTarget{target}}
				if i < len(values) {
					group.Values = []ast.Expression{values[i]}
				}
				groups = append(groups, group)
				order++
			}
		}
	}
	return groups
}

func (l *lowerer) globalInitTargets(decl ast.Decl) []globalInitTarget {
	declaredType := l.resolveSourceType(decl.Var.Type)
	typ := declaredType
	if typ == "" {
		typ = "Any"
	}
	scope := newFuncScope(nil, nil)
	targets := make([]globalInitTarget, 0, len(decl.Var.Names))
	for i, name := range decl.Var.Names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		blank := isBlankIdentifier(name)
		globalType := typ
		if declaredType == "" {
			if inferred := l.typeRefString(l.globalTypes[name]); inferred != "" {
				globalType = inferred
			}
		}
		if declaredType == "" && i < len(decl.Var.Values) {
			valueType := l.defaultedExpressionType(decl.Var.Values[i], &scope)
			if valueType != "" {
				globalType = valueType
			}
		} else if declaredType == "" && len(decl.Var.Values) == 1 {
			if valueType := l.multiResultValueType(decl.Var.Values[0], i, &scope); valueType != "" {
				globalType = valueType
			}
		}
		targets = append(targets, globalInitTarget{
			Name:  name,
			Blank: blank,
			Type:  globalType,
		})
	}
	return targets
}

func (l *lowerer) declareGlobalDecl(decl ast.Decl, program *ir.Program) bool {
	targets := l.globalInitTargets(decl)
	for _, target := range targets {
		if target.Blank {
			continue
		}
		program.Globals = append(program.Globals, ir.Global{
			ID:   globalID(target.Name),
			Name: target.Name,
			Type: l.hirType(target.Type),
		})
		l.globalTypes[target.Name] = l.hirType(target.Type)
		l.globalVariadics[target.Name] = target.Variadic
		if isExported(target.Name) {
			program.Exports = append(program.Exports, ir.Export{
				Name: target.Name,
				Kind: "global",
				ID:   globalID(target.Name),
				Type: l.hirType(target.Type),
			})
		}
	}
	return true
}

func (l *lowerer) lowerGlobalInitGroups(groups []globalInitGroup, scope *funcScope) ([]ir.Statement, bool) {
	initialized := map[string]struct{}{}
	pending := make([]bool, len(groups))
	for i := range pending {
		pending[i] = true
	}
	out := []ir.Statement{}
	remaining := len(groups)
	for remaining > 0 {
		selected := -1
		for i, group := range groups {
			if !pending[i] || !l.globalInitGroupReady(group, initialized) {
				continue
			}
			selected = i
			break
		}
		if selected < 0 {
			for i, group := range groups {
				if pending[i] {
					l.add("hirgen.init.cycle", "package variable initialization cycle", group.Decl.Span)
					break
				}
			}
			return nil, false
		}
		stmts, ok := l.lowerGlobalInitGroup(groups[selected], scope)
		if !ok {
			return nil, false
		}
		out = append(out, stmts...)
		for _, target := range groups[selected].Targets {
			if !target.Blank {
				initialized[target.Name] = struct{}{}
			}
		}
		pending[selected] = false
		remaining--
	}
	return out, true
}

func (l *lowerer) globalInitGroupReady(group globalInitGroup, initialized map[string]struct{}) bool {
	for dep := range group.Deps {
		if _, ok := initialized[dep]; !ok {
			return false
		}
	}
	return true
}

func (l *lowerer) lowerGlobalInitGroup(group globalInitGroup, scope *funcScope) ([]ir.Statement, bool) {
	if len(group.Values) == 0 {
		return nil, true
	}
	if l.resolveSourceType(group.Decl.Var.Type) == "" {
		for _, value := range group.Values {
			if isNilLiteral(value) {
				l.add("hirgen.var.nil", "variable initializer cannot use untyped nil without an explicit type", value.Span)
				return nil, false
			}
		}
	}
	if len(group.Targets) > 1 && len(group.Values) == 1 {
		value, ok := l.lowerMultiResultValue(group.Values[0], len(group.Targets), scope, "hirgen.var.results")
		if !ok {
			return nil, false
		}
		targetTypes := make([]string, len(group.Targets))
		for i, target := range group.Targets {
			targetTypes[i] = target.Type
		}
		if !l.validateMultiResultTargets(value, targetTypes, "hirgen.assign.type", group.Values[0]) {
			return nil, false
		}
		if !multiResultNeedsNormalization(value.types, targetTypes) {
			return []ir.Statement{{Kind: ir.StmtStoreResults, Expr: value.expr, Targets: globalStoreTargets(group.Targets)}}, true
		}
		binding, ok := l.bindMultiResult(value, targetTypes, scope, "hirgen.assign.type", group.Values[0])
		if !ok {
			return nil, false
		}
		return []ir.Statement{
			binding.capture(),
			{Kind: ir.StmtStoreValues, Values: binding.values, Targets: globalStoreTargets(group.Targets)},
		}, true
	}
	values := make([]ir.Expression, 0, len(group.Values))
	for i, value := range group.Values {
		targetType := "Any"
		if i < len(group.Targets) {
			targetType = group.Targets[i].Type
		}
		expr, ok := l.lowerExpressionInType(value, targetType, scope)
		if !ok {
			return nil, false
		}
		values = append(values, expr)
	}
	if hirResultsCount(values) != len(group.Targets) {
		l.add("hirgen.var.shape", "var declaration initializer result count must match names", group.Decl.Span)
		return nil, false
	}
	if len(values) == 1 && len(group.Targets) == 1 {
		if group.Targets[0].Blank {
			return []ir.Statement{{Kind: ir.StmtExpr, Expr: values[0]}}, true
		}
		return []ir.Statement{{
			Kind:   ir.StmtStoreGlobal,
			Global: globalID(group.Targets[0].Name),
			Expr:   values[0],
		}}, true
	}
	return []ir.Statement{{Kind: ir.StmtStoreValues, Values: values, Targets: globalStoreTargets(group.Targets)}}, true
}

func globalStoreTargets(targets []globalInitTarget) []ir.StoreTarget {
	out := make([]ir.StoreTarget, 0, len(targets))
	for _, target := range targets {
		if target.Blank {
			out = append(out, ir.StoreTarget{Kind: "discard"})
			continue
		}
		out = append(out, ir.StoreTarget{Kind: "global", Global: globalID(target.Name)})
	}
	return out
}

func (l *lowerer) lowerFuncDecl(decl ast.Decl, overrideID string) (ir.Function, bool) {
	name := decl.Func.Name
	id := functionID(name)
	if strings.TrimSpace(overrideID) != "" {
		id = overrideID
	}
	if decl.Func.Receiver != nil && strings.TrimSpace(overrideID) == "" {
		id = methodID(l.methodReceiverType(decl.Func.Receiver.Type), name)
	}
	fn := ir.Function{
		ID:            id,
		Name:          name,
		RevisionLocal: strings.TrimSpace(overrideID) != "",
		Declaration:   hirLocationPtr(decl.Span),
		Signature:     l.hirSignature(l.signatureOf(decl.Func), funcDeclVariadic(decl.Func)),
	}
	scope := newFuncScope(&fn, nil)
	scope.resultTypes = l.typeRefsFromStrings(l.resultTypes(decl.Func.Results))
	scope.expectedReturns = len(decl.Func.Results)
	if decl.Func.Receiver != nil {
		receiverName := strings.TrimSpace(decl.Func.Receiver.Name)
		receiverType := l.methodReceiverType(decl.Func.Receiver.Type)
		receiverID := localID(receiverName)
		if receiverName == "" || isBlankIdentifier(receiverName) {
			receiverID = receiverLocalID()
		}
		local := ir.Local{
			ID:          receiverID,
			Name:        receiverName,
			Type:        fn.Signature.Params[0].Type,
			Scope:       scope.debugScope,
			Declaration: hirLocationPtr(decl.Func.Receiver.Span),
		}
		fn.Locals = append(fn.Locals, local)
		if receiverName != "" && !isBlankIdentifier(receiverName) {
			scope.locals[receiverName] = local.ID
			scope.localTypes[receiverName] = local.Type
			scope.localVariadics[receiverName] = l.functionTypeVariadic(receiverType)
		}
	}
	for _, param := range decl.Func.Params {
		if !l.addFunctionParamLocal(&fn, &scope, param, len(fn.Locals)) {
			return ir.Function{}, false
		}
	}
	l.collectNamedResults(decl.Func.Results, &fn, &scope)
	body, ok := l.lowerBlockInScope(decl.Func.Body, &scope)
	if !ok {
		return ir.Function{}, false
	}
	fn.Body = append(fn.Body, body...)
	return fn, true
}

func (l *lowerer) collectNamedResults(results []ast.Field, fn *ir.Function, scope *funcScope) {
	if fn == nil || scope == nil {
		return
	}
	for _, result := range results {
		name := strings.TrimSpace(result.Name)
		if name == "" {
			continue
		}
		blank := isBlankIdentifier(name)
		if !blank {
			if _, exists := scope.locals[name]; exists {
				l.add("hirgen.result.name.duplicate", "duplicate named result", result.Span)
				continue
			}
		}
		resultType := l.resolveSourceType(result.Type)
		if resultType == "" {
			resultType = "Any"
		}
		id := localID(name)
		if blank {
			id = resultLocalID(len(fn.ResultLocals))
		}
		local := ir.Local{
			ID:          id,
			Name:        name,
			Type:        l.hirType(resultType),
			Scope:       scope.debugScope,
			Declaration: hirLocationPtr(result.Span),
		}
		fn.Locals = append(fn.Locals, local)
		fn.ResultLocals = append(fn.ResultLocals, local.ID)
		scope.namedResults = append(scope.namedResults, ir.Expression{Kind: ir.ExprLocal, Local: local.ID})
		if blank {
			continue
		}
		scope.locals[name] = local.ID
		scope.localTypes[name] = local.Type
		scope.localVariadics[name] = l.functionTypeVariadic(resultType)
		scope.namedResultLocals[name] = local.ID
	}
}

func (l *lowerer) addFunctionParamLocal(fn *ir.Function, scope *funcScope, param ast.Field, index int) bool {
	if fn == nil || scope == nil {
		return false
	}
	paramType := l.fieldTypeString(param)
	if paramType == "" {
		paramType = "Any"
	}
	name := strings.TrimSpace(param.Name)
	id := fmt.Sprintf("param.%d", index)
	if name != "" && !isBlankIdentifier(name) {
		if _, exists := scope.locals[name]; exists {
			l.add("hirgen.param.duplicate", "duplicate function parameter name", param.Span)
			return false
		}
		id = localID(name)
	}
	local := ir.Local{
		ID:          id,
		Name:        name,
		Type:        l.hirType(paramType),
		Scope:       scope.debugScope,
		Declaration: hirLocationPtr(param.Span),
	}
	fn.Locals = append(fn.Locals, local)
	if name == "" || isBlankIdentifier(name) {
		return true
	}
	scope.locals[name] = local.ID
	scope.localTypes[name] = local.Type
	variadic := l.functionTypeVariadic(paramType)
	scope.localVariadics[name] = variadic
	l.recordFunctionValueType(scope, name, paramType, variadic)
	return true
}

func (l *lowerer) childScope(parent *funcScope) *funcScope {
	if parent == nil {
		child := newFuncScope(nil, nil)
		return &child
	}
	child := newFuncScope(parent.function, parent)
	child.namedResults = append([]ir.Expression(nil), parent.namedResults...)
	for name, local := range parent.namedResultLocals {
		child.namedResultLocals[name] = local
	}
	child.resultTypes = append([]types.TypeRef(nil), parent.resultTypes...)
	child.expectedReturns = parent.expectedReturns
	child.rangeReturn = parent.rangeReturn
	child.deferOwnerDepth = parent.deferOwnerDepth
	return &child
}

func (l *lowerer) lowerBlock(block ast.BlockStmt, scope *funcScope) ([]ir.Statement, bool) {
	blockScope := l.childScope(scope)
	return l.lowerBlockInScope(block, blockScope)
}

func (l *lowerer) lowerBlockInScope(block ast.BlockStmt, blockScope *funcScope) ([]ir.Statement, bool) {
	previousFutureConsts := blockScope.futureConsts
	blockScope.futureConsts = blockLocalConstNames(block)
	defer func() {
		blockScope.futureConsts = previousFutureConsts
	}()
	var out []ir.Statement
	for _, stmt := range block.Stmts {
		lowered, ok := l.lowerStatement(stmt, blockScope)
		if !ok {
			return nil, false
		}
		if loc := hirLocationPtr(stmt.Span); loc != nil && len(lowered) != 0 && len(lowered[0].SourcePoints) == 0 {
			lowered[0].SourcePoints = []ir.Location{*loc}
		}
		for index := range lowered {
			if lowered[index].Scope == 0 {
				lowered[index].Scope = blockScope.debugScope
			}
		}
		out = append(out, lowered...)
	}
	return out, true
}

func blockLocalConstNames(block ast.BlockStmt) map[string]int {
	out := map[string]int{}
	for _, stmt := range block.Stmts {
		if stmt.Kind != ast.StmtDecl {
			continue
		}
		for _, decl := range statementDeclPtrs(&stmt) {
			if decl == nil || decl.Kind != ast.DeclConst {
				continue
			}
			for _, name := range decl.Const.Names {
				name = strings.TrimSpace(name)
				if name == "" || isBlankIdentifier(name) {
					continue
				}
				out[name]++
			}
		}
	}
	return out
}

func lowerDebugFiles(files []ast.File) []ir.SourceFile {
	out := make([]ir.SourceFile, 0, len(files))
	seen := map[string]struct{}{}
	for _, file := range files {
		if strings.TrimSpace(file.Path) == "" {
			continue
		}
		id := strings.TrimSpace(file.ID)
		if id == "" {
			id = file.Path
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, ir.SourceFile{ID: id, Path: file.Path, Hash: file.Hash})
	}
	return out
}

func lowerDebugSourceHash(files []ast.File) string {
	inputFiles := make([]source.File, 0, len(files))
	for _, file := range files {
		id := strings.TrimSpace(file.ID)
		if id == "" {
			id = file.Path
		}
		inputFiles = append(inputFiles, source.File{
			ID:   id,
			Path: file.Path,
			Hash: file.Hash,
		})
	}
	return source.HashFiles(inputFiles)
}

func hirLocationPtr(span source.Span) *ir.Location {
	if !span.Valid() {
		return nil
	}
	loc := new(ir.Location)
	loc.File = span.Start.File
	loc.Line = span.Start.Line
	loc.Column = span.Start.Column
	return loc
}
