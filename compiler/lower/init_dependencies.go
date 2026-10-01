package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

type initReferenceSet struct {
	Globals   map[string]struct{}
	Functions map[string]struct{}
	Methods   map[string]struct{}
}

func newInitReferenceSet() initReferenceSet {
	return initReferenceSet{
		Globals:   map[string]struct{}{},
		Functions: map[string]struct{}{},
		Methods:   map[string]struct{}{},
	}
}

func (l *lowerer) packageFunctionDependencies(program ast.Program) map[string]map[string]struct{} {
	refs := map[string]initReferenceSet{}
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclFunc {
				continue
			}
			name := strings.TrimSpace(decl.Func.Name)
			if name == "" || name == "init" || isBlankIdentifier(name) {
				continue
			}
			key := name
			if decl.Func.Receiver != nil {
				receiver := l.methodReceiverType(decl.Func.Receiver.Type)
				key = methodID(receiver, name)
			}
			refs[key] = l.collectFunctionInitReferences(*decl.Func)
		}
	}
	deps := map[string]map[string]struct{}{}
	resolved := map[string]struct{}{}
	for key := range refs {
		l.addTransitiveInitDependencies(key, refs, deps, resolved, map[string]struct{}{})
	}
	return deps
}

func (l *lowerer) globalInitDependencies(values []ast.Expression, functionDeps map[string]map[string]struct{}) map[string]struct{} {
	refs := newInitReferenceSet()
	scope := newInitDependencyScope(nil)
	for _, value := range values {
		l.collectExpressionInitReferences(value, scope, &refs)
	}
	deps := map[string]struct{}{}
	for global := range refs.Globals {
		deps[global] = struct{}{}
	}
	for function := range refs.Functions {
		for global := range functionDeps[function] {
			deps[global] = struct{}{}
		}
	}
	for method := range refs.Methods {
		for global := range functionDeps[method] {
			deps[global] = struct{}{}
		}
	}
	return deps
}

func (l *lowerer) addTransitiveInitDependencies(
	key string,
	refs map[string]initReferenceSet,
	deps map[string]map[string]struct{},
	resolved map[string]struct{},
	visiting map[string]struct{},
) {
	if _, ok := deps[key]; !ok {
		deps[key] = map[string]struct{}{}
	}
	if _, ok := resolved[key]; ok {
		return
	}
	if _, recursive := visiting[key]; recursive {
		return
	}
	ref, ok := refs[key]
	if !ok {
		resolved[key] = struct{}{}
		return
	}
	visiting[key] = struct{}{}
	for global := range ref.Globals {
		deps[key][global] = struct{}{}
	}
	for function := range ref.Functions {
		l.addTransitiveInitDependencies(function, refs, deps, resolved, visiting)
		for global := range deps[function] {
			deps[key][global] = struct{}{}
		}
	}
	for method := range ref.Methods {
		l.addTransitiveInitDependencies(method, refs, deps, resolved, visiting)
		for global := range deps[method] {
			deps[key][global] = struct{}{}
		}
	}
	delete(visiting, key)
	resolved[key] = struct{}{}
}

type initDependencyScope struct {
	parent     *initDependencyScope
	locals     map[string]string
	constants  map[string]struct{}
	localTypes map[string]string
}

func newInitDependencyScope(parent *initDependencyScope) *initDependencyScope {
	return &initDependencyScope{
		parent:     parent,
		locals:     map[string]string{},
		constants:  map[string]struct{}{},
		localTypes: map[string]string{},
	}
}

func (s *initDependencyScope) child() *initDependencyScope {
	return newInitDependencyScope(s)
}

func (s *initDependencyScope) declareValue(name, typ string) {
	name = strings.TrimSpace(name)
	if name == "" || isBlankIdentifier(name) {
		return
	}
	s.locals[name] = strings.TrimSpace(typ)
	delete(s.constants, name)
}

func (s *initDependencyScope) declareConst(name string) {
	name = strings.TrimSpace(name)
	if name == "" || isBlankIdentifier(name) {
		return
	}
	s.constants[name] = struct{}{}
	delete(s.locals, name)
}

func (s *initDependencyScope) localType(name string) (string, bool) {
	for current := s; current != nil; current = current.parent {
		if typ, ok := current.locals[name]; ok {
			return typ, true
		}
	}
	return "", false
}

func (s *initDependencyScope) hasLocal(name string) bool {
	_, ok := s.localType(name)
	if ok {
		return true
	}
	for current := s; current != nil; current = current.parent {
		if _, ok := current.constants[name]; ok {
			return true
		}
	}
	return false
}

func (l *lowerer) collectFunctionInitReferences(fn ast.FuncDecl) initReferenceSet {
	refs := newInitReferenceSet()
	scope := newInitDependencyScope(nil)
	if fn.Receiver != nil {
		scope.declareValue(fn.Receiver.Name, l.resolveSourceType(fn.Receiver.Type))
	}
	l.declareInitDependencyFields(fn.Params, scope)
	l.declareInitDependencyFields(fn.Results, scope)
	l.collectBlockInitReferences(fn.Body, scope, &refs)
	return refs
}

func (l *lowerer) declareInitDependencyFields(fields []ast.Field, scope *initDependencyScope) {
	for _, field := range fields {
		scope.declareValue(field.Name, l.resolveSourceType(field.Type))
	}
}

func (l *lowerer) collectBlockInitReferences(block ast.BlockStmt, scope *initDependencyScope, refs *initReferenceSet) {
	blockScope := scope.child()
	for _, stmt := range block.Stmts {
		l.collectStatementInitReferences(stmt, blockScope, refs)
	}
}

func (l *lowerer) collectStatementInitReferences(stmt ast.Statement, scope *initDependencyScope, refs *initReferenceSet) {
	switch stmt.Kind {
	case ast.StmtDecl:
		l.collectDeclStatementInitReferences(stmt, scope, refs)
	case ast.StmtExpr, ast.StmtPanic:
		if stmt.Expr != nil {
			l.collectExpressionInitReferences(*stmt.Expr, scope, refs)
		}
	case ast.StmtAssign:
		if stmt.Op == ":=" {
			for _, expr := range stmt.Right {
				l.collectExpressionInitReferences(expr, scope, refs)
			}
			for i, target := range stmt.Left {
				if target.Kind == ast.ExprIdent {
					typ := ""
					if i < len(stmt.Right) {
						typ = l.dependencyExpressionType(stmt.Right[i], scope)
					}
					scope.declareValue(target.Name, typ)
					continue
				}
				l.collectExpressionInitReferences(target, scope, refs)
			}
			return
		}
		for _, target := range stmt.Left {
			l.collectExpressionInitReferences(target, scope, refs)
		}
		for _, expr := range stmt.Right {
			l.collectExpressionInitReferences(expr, scope, refs)
		}
	case ast.StmtReturn:
		for _, expr := range stmt.Results {
			l.collectExpressionInitReferences(expr, scope, refs)
		}
	case ast.StmtIf:
		if stmt.Init != nil {
			l.collectStatementInitReferences(*stmt.Init, scope.child(), refs)
		}
		if stmt.Cond != nil {
			l.collectExpressionInitReferences(*stmt.Cond, scope, refs)
		}
		l.collectBlockInitReferences(stmt.Body, scope, refs)
		if stmt.Else != nil {
			l.collectStatementInitReferences(*stmt.Else, scope.child(), refs)
		}
	case ast.StmtFor:
		loopScope := scope.child()
		if stmt.Init != nil {
			l.collectStatementInitReferences(*stmt.Init, loopScope, refs)
		}
		if stmt.Cond != nil {
			l.collectExpressionInitReferences(*stmt.Cond, loopScope, refs)
		}
		if stmt.Post != nil {
			l.collectStatementInitReferences(*stmt.Post, loopScope, refs)
		}
		l.collectBlockInitReferences(stmt.Body, loopScope, refs)
	case ast.StmtRange:
		rangeScope := scope.child()
		if stmt.Range != nil {
			l.collectExpressionInitReferences(*stmt.Range, rangeScope, refs)
		}
		if stmt.Op == ":=" {
			if stmt.Key != nil && stmt.Key.Kind == ast.ExprIdent {
				rangeScope.declareValue(stmt.Key.Name, "Int")
			}
			if stmt.Value != nil && stmt.Value.Kind == ast.ExprIdent {
				rangeScope.declareValue(stmt.Value.Name, "")
			}
		} else {
			if stmt.Key != nil {
				l.collectExpressionInitReferences(*stmt.Key, rangeScope, refs)
			}
			if stmt.Value != nil {
				l.collectExpressionInitReferences(*stmt.Value, rangeScope, refs)
			}
		}
		l.collectBlockInitReferences(stmt.Body, rangeScope, refs)
	case ast.StmtSwitch:
		switchScope := scope.child()
		if stmt.Init != nil {
			l.collectStatementInitReferences(*stmt.Init, switchScope, refs)
		}
		if stmt.Expr != nil {
			l.collectExpressionInitReferences(*stmt.Expr, switchScope, refs)
		}
		if stmt.Cond != nil {
			l.collectExpressionInitReferences(*stmt.Cond, switchScope, refs)
		}
		for _, clause := range stmt.Cases {
			for _, expr := range clause.Values {
				l.collectExpressionInitReferences(expr, switchScope, refs)
			}
			l.collectBlockInitReferences(clause.Body, switchScope, refs)
		}
	case ast.StmtSelect:
		for _, clause := range stmt.Cases {
			caseScope := scope.child()
			if clause.Comm != nil {
				l.collectStatementInitReferences(*clause.Comm, caseScope, refs)
			}
			l.collectBlockInitReferences(clause.Body, caseScope, refs)
		}
	case ast.StmtSend:
		for _, expr := range stmt.Left {
			l.collectExpressionInitReferences(expr, scope, refs)
		}
		for _, expr := range stmt.Right {
			l.collectExpressionInitReferences(expr, scope, refs)
		}
	case ast.StmtBlock, ast.StmtLabel:
		l.collectBlockInitReferences(stmt.Body, scope, refs)
	case ast.StmtDefer, ast.StmtGo:
		if stmt.Expr != nil {
			l.collectExpressionInitReferences(*stmt.Expr, scope, refs)
		}
	}
}

func (l *lowerer) collectDeclStatementInitReferences(stmt ast.Statement, scope *initDependencyScope, refs *initReferenceSet) {
	for _, decl := range stmt.Decls {
		switch decl.Kind {
		case ast.DeclConst:
			for _, value := range decl.Const.Values {
				l.collectExpressionInitReferences(value, scope, refs)
			}
			for _, name := range decl.Const.Names {
				scope.declareConst(name)
			}
		case ast.DeclVar:
			for _, value := range decl.Var.Values {
				l.collectExpressionInitReferences(value, scope, refs)
			}
			for i, name := range decl.Var.Names {
				scope.declareValue(name, l.dependencyValueDeclType(*decl.Var, i, scope))
			}
		case ast.DeclType:
			name := strings.TrimSpace(decl.Type.Name)
			if name != "" && !isBlankIdentifier(name) {
				scope.localTypes[name] = l.resolveSourceType(decl.Type.Type)
			}
		case ast.DeclFunc:
			name := strings.TrimSpace(decl.Func.Name)
			if name != "" && !isBlankIdentifier(name) {
				scope.declareValue(name, l.signatureOf(*decl.Func))
			}
		}
	}
}

func (l *lowerer) dependencyValueDeclType(decl ast.ValueDecl, index int, scope *initDependencyScope) string {
	if decl.Type.Kind != ast.TypeInvalid {
		return l.resolveSourceType(decl.Type)
	}
	if index >= 0 && index < len(decl.Values) && len(decl.Values) == len(decl.Names) {
		return l.dependencyExpressionType(decl.Values[index], scope)
	}
	return ""
}

func (l *lowerer) collectExpressionInitReferences(expr ast.Expression, scope *initDependencyScope, refs *initReferenceSet) {
	switch expr.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if name == "" || isBlankIdentifier(name) || scope.hasLocal(name) {
			return
		}
		if _, ok := l.globals[name]; ok {
			refs.Globals[name] = struct{}{}
		}
		if _, ok := l.functions[name]; ok {
			refs.Functions[name] = struct{}{}
		}
	case ast.ExprUnary, ast.ExprAddr, ast.ExprDeref, ast.ExprReceive:
		if expr.Operand != nil {
			l.collectExpressionInitReferences(*expr.Operand, scope, refs)
		}
	case ast.ExprBinary:
		if expr.Left != nil {
			l.collectExpressionInitReferences(*expr.Left, scope, refs)
		}
		if expr.Right != nil {
			l.collectExpressionInitReferences(*expr.Right, scope, refs)
		}
	case ast.ExprCall:
		if expr.Callee != nil {
			l.collectExpressionInitReferences(*expr.Callee, scope, refs)
		}
		for _, arg := range expr.Args {
			l.collectExpressionInitReferences(arg, scope, refs)
		}
	case ast.ExprSelector:
		if expr.Operand != nil {
			l.collectExpressionInitReferences(*expr.Operand, scope, refs)
			l.collectSelectorMethodInitReference(expr, refs)
		}
	case ast.ExprIndex:
		if expr.Operand != nil {
			l.collectExpressionInitReferences(*expr.Operand, scope, refs)
		}
		if expr.Index != nil {
			l.collectExpressionInitReferences(*expr.Index, scope, refs)
		}
	case ast.ExprSlice:
		if expr.Operand != nil {
			l.collectExpressionInitReferences(*expr.Operand, scope, refs)
		}
		if expr.Start != nil {
			l.collectExpressionInitReferences(*expr.Start, scope, refs)
		}
		if expr.End != nil {
			l.collectExpressionInitReferences(*expr.End, scope, refs)
		}
		if expr.Max != nil {
			l.collectExpressionInitReferences(*expr.Max, scope, refs)
		}
	case ast.ExprComposite:
		literalFields := l.structLiteralFields(expr.Type, l.resolveSourceTypePtr(expr.Type, nil))
		for i, item := range expr.Items {
			if item.Key == nil && len(literalFields) == len(expr.Items) && literalFields[i].name == "_" {
				continue
			}
			if item.Key != nil {
				l.collectExpressionInitReferences(*item.Key, scope, refs)
			}
			l.collectExpressionInitReferences(item.Value, scope, refs)
		}
	case ast.ExprFunc:
		l.collectFunctionLiteralInitReferences(*expr.Func, scope, refs)
	case ast.ExprConvert, ast.ExprAssert:
		if expr.Operand != nil {
			l.collectExpressionInitReferences(*expr.Operand, scope, refs)
		}
	}
}

func (l *lowerer) collectSelectorMethodInitReference(expr ast.Expression, refs *initReferenceSet) {
	if expr.Operand == nil || strings.TrimSpace(expr.Field) == "" {
		return
	}
	selection, selected := l.semanticSelection(expr)
	if !selected || selection.Kind != check.SelectionMethod || selection.Interface {
		return
	}
	method, _, ok := l.semanticMethodInfo(expr)
	if !ok || strings.TrimSpace(method.ModulePath) != "" {
		return
	}
	refs.Methods[method.FunctionID] = struct{}{}
}

func (l *lowerer) collectFunctionLiteralInitReferences(fn ast.FuncDecl, parent *initDependencyScope, refs *initReferenceSet) {
	scope := parent.child()
	l.declareInitDependencyFields(fn.Params, scope)
	l.declareInitDependencyFields(fn.Results, scope)
	l.collectBlockInitReferences(fn.Body, scope, refs)
}

func (l *lowerer) dependencyExpressionType(expr ast.Expression, scope *initDependencyScope) string {
	switch expr.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if typ, ok := scope.localType(name); ok {
			return l.resolveType(typ)
		}
		if typ, ok := l.globalTypes[name]; ok {
			return l.resolveType(l.typeRefString(typ))
		}
	case ast.ExprComposite, ast.ExprConvert, ast.ExprAssert:
		return l.resolveSourceTypePtr(expr.Type, nil)
	case ast.ExprAddr:
		if expr.Operand == nil {
			return ""
		}
		if typ := l.dependencyExpressionType(*expr.Operand, scope); typ != "" {
			return "Ptr<" + typ + ">"
		}
	case ast.ExprDeref:
		if expr.Operand == nil {
			return ""
		}
		elem, _ := l.pointerElementType(l.dependencyExpressionType(*expr.Operand, scope))
		return elem
	case ast.ExprSelector:
		if expr.Operand == nil {
			return ""
		}
		return l.memberType(l.dependencyExpressionType(*expr.Operand, scope), expr.Field)
	case ast.ExprIndex:
		if expr.Operand == nil {
			return ""
		}
		return l.indexElementType(l.dependencyExpressionType(*expr.Operand, scope))
	case ast.ExprCall:
		if results, ok := l.semanticExpressionResults(expr); ok && len(results) == 1 {
			return results[0]
		}
		if expr.Callee != nil {
			if results, ok := l.dependencyCallResultTypes(*expr.Callee, scope); ok && len(results) == 1 {
				return results[0]
			}
		}
	case ast.ExprLiteral:
		return l.resolveSourceTypePtr(expr.Type, nil)
	}
	return ""
}

func (l *lowerer) dependencyCallResultTypes(callee ast.Expression, scope *initDependencyScope) ([]string, bool) {
	switch callee.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(callee.Name)
		if signature, ok := l.semanticFunctionSignature(name); ok {
			return l.signatureResultTypes(signature), true
		}
		if typ, ok := scope.localType(name); ok {
			if results, ok := l.functionSignatureResultTypes(typ); ok {
				return results, true
			}
		}
	}
	return nil, false
}
