package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) selectorModulePath(expr ast.Expression, scope *funcScope) (string, bool) {
	if expr.Kind != ast.ExprSelector || expr.Operand == nil || expr.Operand.Kind != ast.ExprIdent {
		return "", false
	}
	name := expr.Operand.Name
	if _, _, ok := l.lookupLocal(name, scope); ok {
		return "", false
	}
	if _, ok := l.resolveUpvalue(name, scope); ok {
		return "", false
	}
	return l.importPathForAlias(name, expr.Operand.Span)
}

func (l *lowerer) importPathForAlias(alias string, span source.Span) (string, bool) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", false
	}
	if file := sourceFileFromSpan(span); file != "" {
		if imports := l.importsByFile[file]; imports != nil {
			if modulePath, ok := imports[alias]; ok {
				return modulePath, true
			}
		}
	}
	modulePath, ok := l.imports[alias]
	return modulePath, ok
}

func sourceFileFromSpan(span source.Span) string {
	if strings.TrimSpace(span.Start.File) != "" {
		return strings.TrimSpace(span.Start.File)
	}
	return strings.TrimSpace(span.End.File)
}

func (l *lowerer) selectorExport(expr ast.Expression, scope *funcScope) (moduleExportInfo, bool) {
	modulePath, ok := l.selectorModulePath(expr, scope)
	if !ok {
		return moduleExportInfo{}, false
	}
	exports := l.moduleExports[modulePath]
	if len(exports) == 0 {
		return moduleExportInfo{}, false
	}
	info, ok := exports[strings.TrimSpace(expr.Field)]
	return info, ok
}

func (l *lowerer) importedVariable(expr ast.Expression) (string, string, bool) {
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok || !info.Category.Addressable() {
		return "", "", false
	}
	if selection := l.semantic.Selections[expr.NodeID]; selection.Kind == check.SelectionPackageMember {
		return selection.ModulePath, selection.Name, true
	}
	if expr.Kind == ast.ExprIdent {
		if object, ok := l.semantic.Object(info.Object); ok && object.Kind == check.ObjectVar && object.ExportName != "" && object.ModulePath != "" {
			return object.ModulePath, object.ExportName, true
		}
	}
	return "", "", false
}

func (l *lowerer) dotImportExport(name string, span source.Span) (moduleExportInfo, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return moduleExportInfo{}, false
	}
	var info moduleExportInfo
	var ok bool
	if file := sourceFileFromSpan(span); file != "" {
		fileDotImports := l.dotImportsByFile[file]
		info, ok = fileDotImports[name]
		if !ok {
			return moduleExportInfo{}, false
		}
	} else {
		info, ok = l.unambiguousDotImports[name]
		if !ok {
			return moduleExportInfo{}, false
		}
	}
	l.markImportExport(info.ModulePath, name)
	l.markImportExportsInType(info.ModulePath, info.Type)
	l.markImportExportsInType(info.ModulePath, info.Underlying)
	return info, true
}

func (l *lowerer) importedTypeCanonicalName(name string, export moduleExportInfo) string {
	name = strings.TrimSpace(name)
	if export.Type != "" && export.Type != name {
		return l.resolveType(export.Type)
	}
	if export.ModulePath == "" || name == "" {
		return ""
	}
	return export.ModulePath + "." + name
}

func (l *lowerer) selectorTypeExport(expr ast.Expression, scope *funcScope) (moduleExportInfo, bool) {
	info, ok := l.selectorExport(expr, scope)
	if !ok || info.Kind != check.ObjectType {
		return moduleExportInfo{}, false
	}
	if modulePath, ok := l.selectorModulePath(expr, scope); ok {
		l.markImportExport(modulePath, expr.Field)
		l.markImportExportsInType(modulePath, info.Type)
		l.markImportExportsInType(modulePath, info.Underlying)
	}
	return info, true
}

func (l *lowerer) selectorConversionType(expr ast.Expression, scope *funcScope) string {
	if expr.Kind == ast.ExprSelector && expr.Operand != nil && expr.Operand.Kind == ast.ExprIdent {
		qualifiedName := strings.TrimSpace(expr.Operand.Name) + "." + strings.TrimSpace(expr.Field)
		if target := l.resolveType(qualifiedName); target != "" {
			return target
		}
	}
	if info, ok := l.selectorTypeExport(expr, scope); ok {
		if info.Type != "" {
			return l.resolveType(info.Type)
		}
		if info.Underlying != "" {
			return l.resolveType(info.Underlying)
		}
	}
	return ""
}
