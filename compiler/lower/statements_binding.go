package lower

import (
	"strings"

	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) currentLocal(name string, scope *funcScope) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || scope == nil {
		return "", false
	}
	if _, ok := scope.locals[name]; !ok {
		return "", false
	}
	return l.typeRefString(scope.localTypes[name]), true
}

func (l *lowerer) lookupLocal(name string, scope *funcScope) (string, string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || scope == nil {
		return "", "", false
	}
	fn := scope.function
	for current := scope; current != nil && current.function == fn; current = current.outer {
		if local, ok := current.locals[name]; ok {
			return local, l.typeRefString(current.localTypes[name]), true
		}
	}
	return "", "", false
}

func (l *lowerer) lookupLocalOrConst(name string, scope *funcScope) (ir.Expression, string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || scope == nil {
		return ir.Expression{}, "", false
	}
	fn := scope.function
	for current := scope; current != nil && current.function == fn; current = current.outer {
		if local, ok := current.locals[name]; ok {
			return ir.Expression{Kind: ir.ExprLocal, Local: local}, l.typeRefString(current.localTypes[name]), true
		}
		if constant, ok := current.constants[name]; ok {
			value := current.constValues[name]
			return ir.Expression{Kind: ir.ExprConst, ConstantID: constant}, value.Type, true
		}
	}
	return ir.Expression{}, "", false
}

func (l *lowerer) lookupConstID(name string, scope *funcScope) (string, string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", false
	}
	for current := scope; current != nil; current = current.outer {
		if constant, ok := current.constants[name]; ok {
			value := current.constValues[name]
			return constant, value.Type, true
		}
		if _, ok := current.locals[name]; ok {
			return "", "", false
		}
	}
	if constant, ok := l.constants[name]; ok {
		value := l.constValues[name]
		return constant, value.Type, true
	}
	return "", "", false
}

func (l *lowerer) lookupConstValue(name string, scope *funcScope) (constantValue, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return constantValue{}, false
	}
	for current := scope; current != nil; current = current.outer {
		if value, ok := current.constValues[name]; ok {
			return value, true
		}
		if _, ok := current.locals[name]; ok {
			return constantValue{}, false
		}
	}
	if value, ok := l.constValues[name]; ok {
		return value, true
	}
	if export, ok := l.dotImportExport(name, source.Span{}); ok && export.Kind == check.ObjectConst && export.Value != nil {
		return constantValue{
			Type:    export.Type,
			Value:   export.Value,
			Untyped: export.Untyped,
		}, true
	}
	return constantValue{}, false
}

func (l *lowerer) lookupUpvalue(name string, scope *funcScope) (string, string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || scope == nil {
		return "", "", false
	}
	fn := scope.function
	for current := scope; current != nil && current.function == fn; current = current.outer {
		if upvalue, ok := current.upvalues[name]; ok {
			return upvalue, l.typeRefString(current.upvalueTypes[name]), true
		}
	}
	return "", "", false
}

func (l *lowerer) lookupBindingVariadic(name string, scope *funcScope) bool {
	name = strings.TrimSpace(name)
	if name == "" || scope == nil {
		return false
	}
	fn := scope.function
	for current := scope; current != nil && current.function == fn; current = current.outer {
		if variadic, ok := current.localVariadics[name]; ok {
			return variadic
		}
		if variadic, ok := current.upvalueVariadics[name]; ok {
			return variadic
		}
	}
	return false
}

func isCompoundAssign(operator string) bool {
	_, ok := compoundOperator(operator)
	return ok
}

func isIncDecAssign(operator string) bool {
	_, ok := incDecCompoundOperator(operator)
	return ok
}

func incDecCompoundOperator(operator string) (string, bool) {
	switch operator {
	case "++":
		return "+=", true
	case "--":
		return "-=", true
	default:
		return "", false
	}
}

func compoundOperator(operator string) (string, bool) {
	switch operator {
	case "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "&^=", "<<=", ">>=":
		return strings.TrimSuffix(operator, "="), true
	default:
		return "", false
	}
}
