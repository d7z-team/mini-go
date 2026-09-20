package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) isInterfaceType(typ any) bool {
	return m.resolvedRuntimeType(typ).ShapeKind() == types.Interface
}

func (m *moduleInstance) nilAssignableRuntimeType(typ any) bool {
	runtimeType := m.resolvedRuntimeType(typ)
	if !runtimeType.Valid() {
		return false
	}
	if runtimeType.Ref.Kind == types.Any || runtimeType.Primitive(types.PrimitiveFunction) {
		return true
	}
	if m.isInterfaceType(runtimeType) {
		return true
	}
	if m.isWaitableTypeName(runtimeType) {
		return true
	}
	switch runtimeType.ShapeKind() {
	case types.Pointer, types.Slice, types.Map, types.Function:
		return true
	}
	return false
}

func (m *moduleInstance) interfaceType(typ string) (string, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return "", false
	}
	revision := uint64(0)
	if m != nil && m.registry != nil {
		revision = m.registry.revision
	}
	if m != nil {
		if cached, ok := m.interfaceTypeCache.load(typ); ok && cached.revision == revision {
			return cached.text, cached.found
		}
	}
	resolved := ""
	found := false
	if m.resolvedRuntimeType(typ).ShapeKind() == types.Interface {
		resolved, found = typ, true
	} else if m != nil {
		if underlying, ok := m.underlyingType(typ); ok && m.resolvedRuntimeType(underlying).ShapeKind() == types.Interface {
			resolved, found = underlying, true
		}
	}
	if m != nil {
		m.interfaceTypeCache.store(typ, typeTextResolution{text: resolved, revision: revision, found: found})
	}
	return resolved, found
}

func (m *moduleInstance) assertInterfaceValue(value vmValue, target string) (vmValue, bool) {
	var ok bool
	var err error
	value, ok, err = m.unwrapDynamicInterfaceValue(value, target)
	if err != nil {
		return vmValue{}, false
	}
	if !ok {
		return vmValue{Type: coerceRuntimeType(target)}, true
	}
	if m.implementsInterface(value.Type, target) {
		return newVMValue(target, value), true
	}
	return vmValue{}, false
}

func (m *moduleInstance) unwrapDynamicInterfaceValue(value vmValue, target string) (vmValue, bool, error) {
	for i := 0; i < 64; i++ {
		switch {
		case value.Type.Ref.Kind == types.Any:
			if value.Data == nil {
				return vmValue{}, false, nil
			}
			inner, ok := value.Data.(vmValue)
			if !ok {
				return vmValue{}, false, errors.New("invalid Any value")
			}
			value = inner
			continue
		case m.isInterfaceType(value.Type):
			if value.Data == nil {
				return vmValue{}, false, nil
			}
			inner, ok := value.Data.(vmValue)
			if !ok {
				return vmValue{}, false, fmt.Errorf("invalid interface value: type=%s data=%T", value.Type, value.Data)
			}
			value = inner
			continue
		default:
			return value, true, nil
		}
	}
	return vmValue{}, false, fmt.Errorf("dynamic value nesting limit exceeded while converting to %s", strings.TrimSpace(target))
}

func (m *moduleInstance) implementsInterface(valueType any, target string) bool {
	resolvedValue := m.resolvedRuntimeType(valueType)
	resolvedTarget := m.resolvedRuntimeType(target)
	valueTypeText := resolvedValue.String()
	if strings.TrimSpace(target) == "Any" {
		return true
	}
	if sameNamedRuntimeType(resolvedValue, resolvedTarget) {
		return true
	}
	if m.sameRuntimeType(valueType, target) {
		return true
	}
	if m.isGeneralInterfaceType(target) || m.isGeneralInterfaceType(valueTypeText) {
		return false
	}
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	cacheKey := valueTypeText + "\x00" + resolvedTarget.String()
	if cached, ok := m.interfaceImplementationCache.load(cacheKey); ok && cached.revision == revision {
		return cached.value
	}
	methods := m.interfaceMethods(target, map[string]struct{}{})
	if len(methods) == 0 {
		if _, ok := m.interfaceType(target); ok {
			m.cacheInterfaceImplementation(cacheKey, revision, true)
			return true
		}
		m.cacheInterfaceImplementation(cacheKey, revision, false)
		return false
	}
	available := m.valueMethodSet(valueTypeText)
	if sourceMethods := m.interfaceMethods(valueTypeText, map[string]struct{}{}); len(sourceMethods) != 0 || m.isInterfaceType(valueType) {
		available = sourceMethods
	}
	for name, signature := range methods {
		if available[name] != signature {
			m.cacheInterfaceImplementation(cacheKey, revision, false)
			return false
		}
	}
	m.cacheInterfaceImplementation(cacheKey, revision, true)
	return true
}

func (m *moduleInstance) cacheInterfaceImplementation(key string, revision uint64, value bool) {
	m.interfaceImplementationCache.store(key, boolResolution{value: value, revision: revision})
}

func sameNamedRuntimeType(left, right vmType) bool {
	return left.Ref.Kind == types.Named && right.Ref.Kind == types.Named && left.Ref.Named == right.Ref.Named
}

func (m *moduleInstance) interfaceMethods(typ string, seen map[string]struct{}) map[string]string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return nil
	}
	if _, recursive := seen[typ]; recursive {
		return nil
	}
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	topLevel := len(seen) == 0
	if topLevel {
		if cached, ok := m.interfaceMethodsCache.load(typ); ok && cached.revision == revision {
			return cached.methods
		}
	}
	seen[typ] = struct{}{}
	defer delete(seen, typ)
	out := map[string]string{}
	if owner, decl, ok := m.interfaceTypeDecl(typ); ok {
		for _, method := range decl.Methods {
			name := strings.TrimSpace(method.Name)
			signature := types.FormatSignature(&owner.executable.Artifact.TypeTable, method.Signature)
			if name == "" || signature == "" {
				continue
			}
			methodOwner := strings.TrimSpace(method.ModulePath)
			if methodOwner == "" {
				methodOwner = owner.modulePath()
			}
			signature = owner.qualifyLocalType(signature)
			out[methodIdentity(methodOwner, name)] = signature
		}
		if topLevel {
			m.cacheInterfaceMethods(typ, revision, out)
		}
		return out
	}
	canonical, ok := m.interfaceType(typ)
	if !ok {
		if topLevel {
			m.cacheInterfaceMethods(typ, revision, nil)
		}
		return nil
	}
	if canonical != typ {
		if _, recursive := seen[canonical]; recursive {
			return nil
		}
		seen[canonical] = struct{}{}
		defer delete(seen, canonical)
	}
	runtimeType := m.resolvedRuntimeType(canonical)
	info, ok := runtimeType.InterfaceInfo()
	if !ok {
		return out
	}
	for _, method := range info.Methods {
		name := strings.TrimSpace(method.Name)
		if name == "" || name == "_" {
			continue
		}
		owner := strings.TrimSpace(method.ModulePath)
		if owner == "" {
			owner = m.modulePath()
		}
		signature := types.FormatSignature(runtimeType.Table, method.Signature)
		if signature != "" {
			out[methodIdentity(owner, name)] = signature
		}
	}
	for _, term := range info.Terms {
		if term.Approx || term.Union {
			continue
		}
		member := strings.TrimSpace(types.FormatWithTable(runtimeType.Table, term.Type))
		if _, ok := m.interfaceType(member); !ok {
			continue
		}
		for identity, signature := range m.interfaceMethods(member, seen) {
			out[identity] = signature
		}
	}
	if topLevel {
		m.cacheInterfaceMethods(typ, revision, out)
	}
	return out
}

func (m *moduleInstance) cacheInterfaceMethods(typ string, revision uint64, methods map[string]string) {
	m.interfaceMethodsCache.store(typ, methodSetResolution{methods: methods, revision: revision})
}

func (m *moduleInstance) interfaceTypeDecl(typ string) (*moduleInstance, types.TypeNode, bool) {
	if m == nil || m.executable == nil {
		return nil, types.TypeNode{}, false
	}
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return nil, types.TypeNode{}, false
	}
	module := m
	name := typ
	if target, localName, ok := m.qualifiedTypeModule(typ); ok {
		module = target
		name = localName
	}
	decl, ok := module.executable.Types[name]
	if !ok {
		return nil, types.TypeNode{}, false
	}
	underlying := module.formatType(decl.Underlying)
	if m.resolvedRuntimeType(underlying).ShapeKind() != types.Interface {
		return nil, types.TypeNode{}, false
	}
	return module, decl, true
}

func (m *moduleInstance) isGeneralInterfaceType(typ string) bool {
	canonical, ok := m.interfaceType(typ)
	if !ok {
		return false
	}
	return m.interfaceHasTypeSet(canonical, map[string]struct{}{})
}

func (m *moduleInstance) interfaceHasTypeSet(canonical string, seen map[string]struct{}) bool {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return false
	}
	if _, recursive := seen[canonical]; recursive {
		return false
	}
	seen[canonical] = struct{}{}
	defer delete(seen, canonical)
	runtimeType := m.resolvedRuntimeType(canonical)
	info, ok := runtimeType.InterfaceInfo()
	if !ok {
		return false
	}
	if info.TypeSet {
		return true
	}
	for _, term := range info.Terms {
		if term.Approx || term.Union {
			return true
		}
		member := strings.TrimSpace(types.FormatWithTable(runtimeType.Table, term.Type))
		if member == "Any" {
			continue
		}
		if nested, ok := m.interfaceType(member); ok {
			if m.interfaceHasTypeSet(nested, seen) {
				return true
			}
			continue
		}
		// A bare canonical member that is not a known interface is a
		// singleton type term. This keeps interface{Int} distinct from
		// interface{Reader} after the compiler has emitted bytecode.
		return true
	}
	return false
}
