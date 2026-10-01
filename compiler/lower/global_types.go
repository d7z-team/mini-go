package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func (l *lowerer) inferPackageGlobalTypes(program ast.Program) {
	for changed := true; changed; {
		changed = false
		for _, file := range program.Files {
			for _, decl := range file.Decls {
				if decl.Kind != ast.DeclVar {
					continue
				}
				if l.inferGlobalDeclTypes(*decl.Var) {
					changed = true
				}
			}
		}
	}
}

func (l *lowerer) inferGlobalDeclTypes(decl ast.ValueDecl) bool {
	declaredType := l.resolveSourceType(decl.Type)
	if declaredType != "" {
		return false
	}
	scope := newFuncScope(nil, nil)
	changed := false
	for i, name := range decl.Names {
		name = strings.TrimSpace(name)
		if name == "" || isBlankIdentifier(name) || l.globalTypes[name].Valid() {
			continue
		}
		typ := ""
		if i < len(decl.Values) {
			typ = l.defaultedExpressionType(decl.Values[i], &scope)
		}
		if typ == "" && len(decl.Values) == 1 {
			typ = l.multiResultValueType(decl.Values[0], i, &scope)
		}
		if typ == "" {
			continue
		}
		l.globalTypes[name] = l.hirType(typ)
		l.globalVariadics[name] = l.valueDeclFunctionVariadic(decl, i, &scope)
		changed = true
	}
	return changed
}
