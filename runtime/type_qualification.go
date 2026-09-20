package runtime

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) methodReceiverTypes(valueType string) (*moduleInstance, []string) {
	module := m
	valueType = strings.TrimSpace(valueType)
	if elem, ok := m.resolvedRuntimeType(valueType).PointerElem(); ok {
		inner := elem.String()
		if target, name, ok := m.qualifiedTypeModule(inner); ok {
			module = target
			return module, []string{"Ptr<" + name + ">", name}
		}
		return module, []string{valueType, inner}
	}
	if target, name, ok := m.qualifiedTypeModule(valueType); ok {
		module = target
		return module, []string{name}
	}
	return module, []string{valueType}
}

func (m *moduleInstance) qualifiedTypeModule(typ string) (*moduleInstance, string, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" || m == nil {
		return nil, "", false
	}
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	if cached, ok := m.qualifiedTypeCache.load(typ); ok && cached.revision == revision {
		return cached.module, cached.name, cached.found
	}
	if m.executable != nil {
		modulePath := strings.TrimSpace(m.executable.Artifact.Module.Path)
		prefix := modulePath + "."
		if modulePath != "" && strings.HasPrefix(typ, prefix) {
			name := strings.TrimSpace(typ[len(prefix):])
			if name != "" && !strings.Contains(name, ".") {
				if _, ok := m.executable.Types[name]; ok {
					m.qualifiedTypeCache.store(typ, qualifiedTypeResolution{module: m, name: name, revision: revision, found: true})
					return m, name, true
				}
			}
		}
	}
	if m.registry == nil {
		m.cacheQualifiedTypeResolution(typ, qualifiedTypeResolution{revision: revision})
		return nil, "", false
	}
	var bestModule *moduleInstance
	var bestPath string
	var bestName string
	for modulePath, module := range m.registry.modules {
		prefix := modulePath + "."
		if !strings.HasPrefix(typ, prefix) || len(modulePath) <= len(bestPath) {
			continue
		}
		name := strings.TrimSpace(typ[len(prefix):])
		if name == "" || strings.Contains(name, ".") {
			continue
		}
		if module == nil || module.executable == nil {
			continue
		}
		if _, ok := module.executable.Types[name]; !ok {
			continue
		}
		bestModule = module
		bestPath = modulePath
		bestName = name
	}
	if bestModule == nil {
		m.cacheQualifiedTypeResolution(typ, qualifiedTypeResolution{revision: revision})
		return nil, "", false
	}
	m.cacheQualifiedTypeResolution(typ, qualifiedTypeResolution{module: bestModule, name: bestName, revision: revision, found: true})
	return bestModule, bestName, true
}

func (m *moduleInstance) cacheQualifiedTypeResolution(typ string, resolution qualifiedTypeResolution) {
	m.qualifiedTypeCache.store(typ, resolution)
}

func (m *moduleInstance) localizeType(typ string) string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return ""
	}
	if m != nil {
		if cached, ok := m.localizeTypeCache.load(typ); ok {
			return cached
		}
	}
	modulePath := ""
	if m != nil && m.executable != nil {
		modulePath = strings.TrimSpace(m.executable.Artifact.Module.Path)
	}
	localized, ok := rewriteRuntimeTypeText(typ, func(ref types.TypeRef) types.TypeRef {
		if ref.Kind == types.Named && modulePath != "" && ref.Named.ModulePath == modulePath {
			ref.Named.ModulePath = ""
		}
		return ref
	})
	if ok {
		if m != nil {
			m.localizeTypeCache.store(typ, localized)
		}
		return localized
	}
	if m != nil {
		m.localizeTypeCache.store(typ, typ)
	}
	return typ
}

func (m *moduleInstance) qualifyLocalType(typ string) string {
	typ = strings.TrimSpace(typ)
	if typ == "" || m == nil || m.executable == nil {
		return typ
	}

	if cached, ok := m.qualifyTypeCache.load(typ); ok {
		return cached
	}

	modulePath := strings.TrimSpace(m.executable.Artifact.Module.Path)
	if modulePath == "" {
		return typ
	}
	qualified, ok := rewriteRuntimeTypeText(typ, func(ref types.TypeRef) types.TypeRef {
		if ref.Kind == types.Named && ref.Named.ModulePath == "" {
			if _, ok := m.executable.Types[string(ref.Named.DeclID)]; ok {
				ref.Named.ModulePath = modulePath
			}
			return ref
		}
		if ref.Kind == types.Primitive {
			name := types.PrimitiveName(ref.Primitive)
			if _, ok := m.executable.Types[name]; ok {
				return types.TypeRef{Kind: types.Named, Named: types.TypeKey{ModulePath: modulePath, DeclID: types.DeclID(name)}}
			}
		}
		return ref
	})
	if ok {
		m.qualifyTypeCache.store(typ, qualified)
		return qualified
	}

	m.qualifyTypeCache.store(typ, typ)
	return typ
}
