package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
)

type pendingConstDecl struct {
	name    string
	typ     string
	raw     *constant.Value
	untyped bool
}

type packageConstDecl struct {
	decl ast.Decl
	deps map[string]struct{}
}

const (
	minInt64Text  = "-9223372036854775808"
	maxInt64Text  = "9223372036854775807"
	minInt64Value = int64(-1 << 63)
	maxInt64Value = int64(1<<63 - 1)
)

func (l *lowerer) lowerPackageConstDecls(program ast.Program, out *ir.Program) bool {
	decls := l.collectPackageConstDecls(program)
	results := make([][]pendingConstDecl, len(decls))
	done := make([]bool, len(decls))
	ready := map[string]struct{}{}
	for name, value := range l.constValues {
		if value.Value != nil {
			ready[name] = struct{}{}
		}
	}
	remaining := len(decls)
	for remaining > 0 {
		selected := -1
		for i, decl := range decls {
			if done[i] || !packageConstDepsReady(decl.deps, ready) {
				continue
			}
			selected = i
			break
		}
		if selected < 0 {
			for i, decl := range decls {
				if !done[i] {
					l.add("hirgen.const.cycle", "constant declaration cycle", decl.decl.Span)
					break
				}
			}
			return false
		}
		constants, ok := l.evaluatePackageConstDecl(decls[selected].decl)
		if !ok {
			return false
		}
		results[selected] = constants
		for _, constant := range constants {
			ready[constant.name] = struct{}{}
		}
		done[selected] = true
		remaining--
	}
	for _, constants := range results {
		for _, constant := range constants {
			l.emitPackageConst(out, constant)
		}
	}
	return true
}

func (l *lowerer) collectPackageConstDecls(program ast.Program) []packageConstDecl {
	var out []packageConstDecl
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind != ast.DeclConst {
				continue
			}
			out = append(out, packageConstDecl{
				decl: decl,
				deps: l.packageConstDependencies(decl),
			})
		}
	}
	return out
}

func packageConstDepsReady(deps, ready map[string]struct{}) bool {
	for dep := range deps {
		if _, ok := ready[dep]; !ok {
			return false
		}
	}
	return true
}

func (l *lowerer) packageConstDependencies(decl ast.Decl) map[string]struct{} {
	deps := map[string]struct{}{}
	sameSpecNames := constSpecNameSet(decl.Const.Names)
	for _, expr := range decl.Const.Values {
		collectPackageConstDependencies(expr, l.constants, sameSpecNames, deps)
	}
	return deps
}

func collectPackageConstDependencies(expr ast.Expression, packageConstants map[string]string, sameSpecNames, deps map[string]struct{}) {
	switch expr.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if _, same := sameSpecNames[name]; same {
			return
		}
		if _, ok := packageConstants[name]; ok {
			deps[name] = struct{}{}
		}
	case ast.ExprUnary:
		if expr.Operand != nil {
			collectPackageConstDependencies(*expr.Operand, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprBinary:
		if expr.Left != nil {
			collectPackageConstDependencies(*expr.Left, packageConstants, sameSpecNames, deps)
		}
		if expr.Right != nil {
			collectPackageConstDependencies(*expr.Right, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprCall:
		if expr.Callee != nil {
			collectPackageConstDependencies(*expr.Callee, packageConstants, sameSpecNames, deps)
		}
		for _, arg := range expr.Args {
			collectPackageConstDependencies(arg, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprSelector:
		if expr.Operand != nil {
			collectPackageConstDependencies(*expr.Operand, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprIndex:
		if expr.Operand != nil {
			collectPackageConstDependencies(*expr.Operand, packageConstants, sameSpecNames, deps)
		}
		if expr.Index != nil {
			collectPackageConstDependencies(*expr.Index, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprSlice:
		if expr.Operand != nil {
			collectPackageConstDependencies(*expr.Operand, packageConstants, sameSpecNames, deps)
		}
		if expr.Start != nil {
			collectPackageConstDependencies(*expr.Start, packageConstants, sameSpecNames, deps)
		}
		if expr.End != nil {
			collectPackageConstDependencies(*expr.End, packageConstants, sameSpecNames, deps)
		}
		if expr.Max != nil {
			collectPackageConstDependencies(*expr.Max, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprComposite:
		for _, item := range expr.Items {
			if item.Key != nil {
				collectPackageConstDependencies(*item.Key, packageConstants, sameSpecNames, deps)
			}
			collectPackageConstDependencies(item.Value, packageConstants, sameSpecNames, deps)
		}
	case ast.ExprConvert, ast.ExprAssert, ast.ExprAddr, ast.ExprDeref, ast.ExprReceive:
		if expr.Operand != nil {
			collectPackageConstDependencies(*expr.Operand, packageConstants, sameSpecNames, deps)
		}
	}
}

func (l *lowerer) evaluatePackageConstDecl(decl ast.Decl) ([]pendingConstDecl, bool) {
	sameSpecNames := constSpecNameSet(decl.Const.Names)
	pending := make([]pendingConstDecl, 0, len(decl.Const.Names))
	for i, name := range decl.Const.Names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if i >= len(decl.Const.Values) {
			l.add("hirgen.const.value.missing", "constant declaration requires a value", decl.Span)
			return nil, false
		}
		expr := decl.Const.Values[i]
		if !l.validateConstSameSpecReferences(expr, sameSpecNames) {
			return nil, false
		}
		if !l.validateConstantArithmetic(expr, nil) {
			return nil, false
		}
		raw, typ, ok := l.constValue(expr, nil)
		if !ok {
			l.add("hirgen.const.literal", "only literal or previously declared named constants are supported in AST emit", expr.Span)
			return nil, false
		}
		if declaredType := l.resolveSourceType(decl.Const.Type); strings.TrimSpace(declaredType) != "" {
			raw, typ, ok = l.convertTypedConstValue(raw, typ, declaredType, expr.Span)
			if !ok {
				return nil, false
			}
		}
		untyped := false
		if decl.Const.Type.Kind == ast.TypeInvalid {
			untyped = l.untypedConstExpression(expr, nil)
		}
		if isBlankIdentifier(name) {
			continue
		}
		pending = append(pending, pendingConstDecl{name: name, typ: typ, raw: raw, untyped: untyped})
	}
	for _, constant := range pending {
		l.constValues[constant.name] = constantValue{
			Type:    constant.typ,
			Value:   constant.raw,
			Untyped: constant.untyped,
		}
	}
	return pending, true
}

func (l *lowerer) emitPackageConst(out *ir.Program, constant pendingConstDecl) {
	out.Constants = append(out.Constants, ir.Constant{
		ID:      constID(constant.name),
		Name:    constant.name,
		Type:    l.hirType(constant.typ),
		Value:   constant.raw,
		Untyped: constant.untyped,
	})
	if isExported(constant.name) {
		out.Exports = append(out.Exports, ir.Export{
			Name:    constant.name,
			Kind:    "const",
			ID:      constID(constant.name),
			Type:    l.hirType(constant.typ),
			Untyped: constant.untyped,
		})
	}
}

func (l *lowerer) lowerLocalConstDecl(decl ast.Decl, scope *funcScope) bool {
	if scope == nil || scope.function == nil {
		l.add("hirgen.const.scope", "local constant declaration requires function scope", decl.Span)
		return false
	}
	if l.program == nil {
		l.add("hirgen.const.program", "local constant declaration requires HIR program", decl.Span)
		return false
	}
	sameSpecNames := constSpecNameSet(decl.Const.Names)
	seen := map[string]struct{}{}
	pending := make([]pendingConstDecl, 0, len(decl.Const.Names))
	l.consumeFutureLocalConstNames(decl.Const.Names, scope)
	for i, name := range decl.Const.Names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		blank := isBlankIdentifier(name)
		if !blank {
			if _, exists := seen[name]; exists {
				l.add("hirgen.local.duplicate", "duplicate local name", decl.Span)
				return false
			}
			seen[name] = struct{}{}
			if _, exists := scope.locals[name]; exists {
				l.add("hirgen.local.duplicate", "duplicate local name", decl.Span)
				return false
			}
			if _, exists := scope.constants[name]; exists {
				l.add("hirgen.local.duplicate", "duplicate local name", decl.Span)
				return false
			}
		}
		if i >= len(decl.Const.Values) {
			l.add("hirgen.const.value.missing", "constant declaration requires a value", decl.Span)
			return false
		}
		expr := decl.Const.Values[i]
		if !l.validateConstSameSpecReferences(expr, sameSpecNames) {
			return false
		}
		if !l.validateConstantArithmetic(expr, scope) {
			return false
		}
		raw, typ, ok := l.constValue(expr, scope)
		if !ok {
			if l.localConstReferencesFuture(expr, scope) {
				l.add("hirgen.const.forward", "local constant initializer cannot reference a later local constant", expr.Span)
				return false
			}
			l.add("hirgen.const.literal", "only literal or previously declared named constants are supported in AST emit", expr.Span)
			return false
		}
		if declaredType := l.resolveSourceType(decl.Const.Type); strings.TrimSpace(declaredType) != "" {
			raw, typ, ok = l.convertTypedConstValue(raw, typ, declaredType, expr.Span)
			if !ok {
				return false
			}
		}
		untyped := false
		if decl.Const.Type.Kind == ast.TypeInvalid {
			untyped = l.untypedConstExpression(expr, scope)
		}
		if blank {
			continue
		}
		pending = append(pending, pendingConstDecl{name: name, typ: typ, raw: raw, untyped: untyped})
	}
	for _, constant := range pending {
		id := l.newConstID(constant.name)
		l.program.Constants = append(l.program.Constants, ir.Constant{
			ID:      id,
			Name:    constant.name,
			Type:    l.hirType(constant.typ),
			Value:   constant.raw,
			Untyped: constant.untyped,
		})
		scope.constants[constant.name] = id
		scope.constValues[constant.name] = constantValue{
			Type:    constant.typ,
			Value:   constant.raw,
			Untyped: constant.untyped,
		}
	}
	return true
}

func (l *lowerer) consumeFutureLocalConstNames(names []string, scope *funcScope) {
	if scope == nil || len(scope.futureConsts) == 0 {
		return
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || isBlankIdentifier(name) {
			continue
		}
		remaining := scope.futureConsts[name] - 1
		if remaining > 0 {
			scope.futureConsts[name] = remaining
			continue
		}
		delete(scope.futureConsts, name)
	}
}

func (l *lowerer) localConstReferencesFuture(expr ast.Expression, scope *funcScope) bool {
	if scope == nil || len(scope.futureConsts) == 0 {
		return false
	}
	names := map[string]struct{}{}
	for name := range scope.futureConsts {
		names[name] = struct{}{}
	}
	return constExprReferencesAnyName(expr, names)
}

func (l *lowerer) convertTypedConstValue(raw *constant.Value, sourceType, targetType string, span source.Span) (*constant.Value, string, bool) {
	targetType = l.resolveType(strings.TrimSpace(targetType))
	if targetType == "" {
		return nil, "", false
	}
	if !l.constRepresentabilityTarget(targetType) {
		return nil, "", false
	}
	sourceType = strings.TrimSpace(sourceType)
	if sourceType == "" || sourceType == "Any" {
		sourceType = inferConstantDefaultType(raw)
	}
	converted, typ, ok := l.convertConstValue(raw, sourceType, targetType)
	if ok {
		return converted, typ, true
	}
	l.add("hirgen.const.representable", "constant value is not representable by target type "+targetType, span)
	return nil, "", false
}

func (l *lowerer) constRepresentabilityTarget(targetType string) bool {
	kind := l.underlyingConstType(targetType)
	return kind == "Bool" || kind == "String" || isNumericType(kind)
}

func inferConstantDefaultType(value *constant.Value) string {
	if value == nil {
		return ""
	}
	switch value.Kind() {
	case constant.Boolean:
		return "Bool"
	case constant.StringValue:
		return "String"
	case constant.ComplexValue:
		return "Complex128"
	case constant.Number:
		if strings.ContainsAny(value.Text, "/.eE") || isFloatType(value.Type) {
			return "Float64"
		}
		return "Int"
	default:
		return ""
	}
}

func defaultUntypedConstType(sourceType string, raw *constant.Value) string {
	sourceType = strings.TrimSpace(sourceType)
	switch sourceType {
	case "Bool", "String":
		return sourceType
	case "Int32":
		return "Int32"
	}
	if isIntegerType(sourceType) {
		return "Int"
	}
	if isFloatType(sourceType) {
		return "Float64"
	}
	if isComplexType(sourceType) {
		return "Complex128"
	}
	return inferConstantDefaultType(raw)
}

func constSpecNameSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || isBlankIdentifier(name) {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

func (l *lowerer) validateConstSameSpecReferences(expr ast.Expression, names map[string]struct{}) bool {
	if len(names) == 0 {
		return true
	}
	if constExprReferencesAnyName(expr, names) {
		l.add("hirgen.const.same_spec", "constant initializer cannot reference a name declared by the same ConstSpec", expr.Span)
		return false
	}
	return true
}

func constExprReferencesAnyName(expr ast.Expression, names map[string]struct{}) bool {
	switch expr.Kind {
	case ast.ExprIdent:
		_, ok := names[strings.TrimSpace(expr.Name)]
		return ok
	case ast.ExprUnary:
		return expr.Operand != nil && constExprReferencesAnyName(*expr.Operand, names)
	case ast.ExprBinary:
		return (expr.Left != nil && constExprReferencesAnyName(*expr.Left, names)) ||
			(expr.Right != nil && constExprReferencesAnyName(*expr.Right, names))
	case ast.ExprCall:
		if expr.Callee != nil && constExprReferencesAnyName(*expr.Callee, names) {
			return true
		}
		for _, arg := range expr.Args {
			if constExprReferencesAnyName(arg, names) {
				return true
			}
		}
	case ast.ExprSelector:
		return expr.Operand != nil && constExprReferencesAnyName(*expr.Operand, names)
	case ast.ExprIndex:
		return (expr.Operand != nil && constExprReferencesAnyName(*expr.Operand, names)) ||
			(expr.Index != nil && constExprReferencesAnyName(*expr.Index, names))
	case ast.ExprSlice:
		return (expr.Operand != nil && constExprReferencesAnyName(*expr.Operand, names)) ||
			(expr.Start != nil && constExprReferencesAnyName(*expr.Start, names)) ||
			(expr.End != nil && constExprReferencesAnyName(*expr.End, names)) ||
			(expr.Max != nil && constExprReferencesAnyName(*expr.Max, names))
	case ast.ExprComposite:
		for _, item := range expr.Items {
			if (item.Key != nil && constExprReferencesAnyName(*item.Key, names)) ||
				constExprReferencesAnyName(item.Value, names) {
				return true
			}
		}
	case ast.ExprConvert, ast.ExprAssert, ast.ExprAddr, ast.ExprDeref, ast.ExprReceive:
		return expr.Operand != nil && constExprReferencesAnyName(*expr.Operand, names)
	}
	return false
}
