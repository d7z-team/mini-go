package runtime

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) resolvedRuntimeType(value any) vmType {
	switch value := value.(type) {
	case vmType:
		if value.Ref.Kind != types.Named || value.hasUnderlying {
			if value.Table != nil || m == nil || m.executable == nil {
				return value
			}
			resolved := runtimeTypeWithTable(value.Ref, &m.executable.Artifact.TypeTable)
			resolved.text = value.text
			return resolved
		}
		if m != nil {
			if cached, ok := m.resolvedRuntimeTypes.load(value.Ref); ok {
				return cached
			}
			resolved := m.resolveNamedRuntimeType(value)

			m.resolvedRuntimeTypes.store(value.Ref, resolved)
			return resolved
		}
		if value.Table != nil || m == nil || m.executable == nil {
			return value
		}
		resolved := runtimeTypeWithTable(value.Ref, &m.executable.Artifact.TypeTable)
		resolved.text = value.text
		return resolved
	case types.TypeRef:
		return m.runtimeType(value)
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return vmType{}
		}
		if m != nil {
			if cached, ok := m.runtimeTypeCache.load(text); ok {
				return m.resolvedRuntimeType(cached)
			}
		}
		if owner, name, ok := m.namedRuntimeType(text); ok && owner != nil && owner.executable != nil {
			if node, found := owner.executable.Types[name]; found {
				if node.Alias && node.AliasTarget.Valid() {
					resolved := owner.resolvedRuntimeType(owner.runtimeType(node.AliasTarget))
					m.cacheRuntimeType(text, resolved)
					return resolved
				}
				resolved := runtimeTypeWithTable(types.TypeRef{Kind: types.Named, Named: types.TypeKey{
					ModulePath: owner.modulePath(), DeclID: types.DeclID(name),
				}}, &owner.executable.Artifact.TypeTable)
				m.cacheRuntimeType(text, resolved)
				return resolved
			}
		}
		parsed := runtimeTypeFromText(text)
		if m != nil && m.executable != nil && parsed.Table == nil {
			parsed.Table = &m.executable.Artifact.TypeTable
		}
		m.cacheRuntimeType(text, parsed)
		return parsed
	default:
		return vmType{}
	}
}

func (m *moduleInstance) resolveNamedRuntimeType(value vmType) vmType {
	if value.Ref.Named.ModulePath != "" && value.Ref.Named.DeclID != "" {
		var owner *moduleInstance
		if m.modulePath() == value.Ref.Named.ModulePath {
			owner = m
		} else if m.registry != nil {
			owner, _ = m.registry.module(value.Ref.Named.ModulePath)
		}
		if owner != nil && owner.executable != nil {
			if node, ok := owner.executable.Types[string(value.Ref.Named.DeclID)]; ok {
				if node.Alias && node.AliasTarget.Valid() {
					return owner.resolvedRuntimeType(owner.runtimeType(node.AliasTarget))
				}
				return runtimeTypeWithTable(canonicalRuntimeTypeRef(value.Ref), &owner.executable.Artifact.TypeTable)
			}
		}
		if value.Table != nil {
			if node, ok := value.Table.Named(value.Ref.Named); ok && (node.Underlying.Valid() || node.AliasTarget.Valid()) {
				return value
			}
		}
	}
	if value.Table != nil || m.executable == nil {
		return value
	}
	resolved := runtimeTypeWithTable(value.Ref, &m.executable.Artifact.TypeTable)
	resolved.text = value.text
	return resolved
}

func (m *moduleInstance) cacheRuntimeType(text string, runtimeType vmType) {
	if m == nil {
		return
	}

	m.runtimeTypeCache.store(text, runtimeType)
}
