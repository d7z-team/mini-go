package lower

import (
	"strings"
	"unicode/utf8"

	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/token"
)

func (l *lowerer) markImportExport(modulePath, export string) {
	modulePath = strings.TrimSpace(modulePath)
	export = strings.TrimSpace(export)
	if modulePath == "" || export == "" {
		return
	}
	exports := l.importExports[modulePath]
	if exports == nil {
		exports = map[string]struct{}{}
		l.importExports[modulePath] = exports
	}
	exports[export] = struct{}{}
}

func (l *lowerer) markImportExportsInType(modulePath, typ string) {
	modulePath = strings.TrimSpace(modulePath)
	typ = strings.TrimSpace(typ)
	if modulePath == "" || typ == "" {
		return
	}
	prefix := modulePath + "."
	for i := 0; i < len(typ); {
		index := strings.Index(typ[i:], prefix)
		if index < 0 {
			return
		}
		start := i + index + len(prefix)
		end := start
		for end < len(typ) {
			ch, size := utf8.DecodeRuneInString(typ[end:])
			if !token.IsIdentifierPart(ch) {
				break
			}
			end += size
		}
		if end > start {
			l.markImportExport(modulePath, typ[start:end])
		}
		i = end
	}
}

func (l *lowerer) sourceRequirements() []ir.Requirement {
	out := make([]ir.Requirement, 0, len(l.importPaths)+len(l.implicitImports))
	seen := map[string]struct{}{}
	for _, modulePath := range l.importPaths {
		modulePath = strings.TrimSpace(modulePath)
		if modulePath == "" {
			continue
		}
		seen[modulePath] = struct{}{}
		exportsSet := l.importExports[modulePath]
		exports := make([]string, 0, len(exportsSet))
		for export := range exportsSet {
			exports = append(exports, export)
		}
		sortStrings(exports)
		out = append(out, ir.Requirement{
			Kind:       "source",
			ModulePath: modulePath,
			Exports:    exports,
		})
	}
	var implicit []string
	for modulePath := range l.implicitImports {
		modulePath = strings.TrimSpace(modulePath)
		if modulePath == "" {
			continue
		}
		if _, ok := seen[modulePath]; ok {
			continue
		}
		implicit = append(implicit, modulePath)
	}
	sortStrings(implicit)
	for _, modulePath := range implicit {
		out = append(out, ir.Requirement{
			Kind:       "source",
			ModulePath: modulePath,
		})
	}
	return out
}

func (l *lowerer) ensureSourceRequirement(modulePath string) {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" {
		return
	}
	for _, existing := range l.importPaths {
		if strings.TrimSpace(existing) == modulePath {
			return
		}
	}
	if l.implicitImports == nil {
		l.implicitImports = map[string]struct{}{}
	}
	l.implicitImports[modulePath] = struct{}{}
}
