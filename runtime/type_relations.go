package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) coerceAssignableValue(value vmValue, target any) (vmValue, error) {
	return m.coerceAssignableRuntimeValue(value, m.resolvedRuntimeType(target), false)
}

func (m *moduleInstance) coerceAssignableRuntimeValue(value vmValue, targetType vmType, targetVariadic bool) (vmValue, error) {
	if !targetType.Valid() {
		return vmValue{}, errors.New("missing target type")
	}
	if targetType.Equal(value.Type) {
		value.Type = targetType
		return value, nil
	}
	targetText := targetType.String()
	if targetType.Ref.Kind == types.Any {
		if value.Type.Ref.Kind == types.Any {
			return value, nil
		}
		return newVMValue(targetType, value), nil
	}
	if (targetType.Ref.Kind == types.Interface || targetType.Ref.Kind == types.Named) && m.isInterfaceType(targetType) {
		out, ok := m.assertInterfaceValue(value, targetText)
		if !ok {
			candidateType := value.Type.String()
			if candidate, present, err := m.unwrapDynamicInterfaceValue(value, targetText); err == nil && present {
				candidateType = candidate.Type.String()
			}
			return vmValue{}, fmt.Errorf("type assertion failed: %s does not implement %s", candidateType, targetText)
		}
		out.Type = targetType
		return out, nil
	}
	if value.Type.Ref.Kind == types.Any {
		if value.Data == nil {
			if m.nilAssignableRuntimeType(targetText) {
				return m.zeroValue(targetType), nil
			}
			return vmValue{}, fmt.Errorf("type assertion failed: nil is not %s", targetText)
		}
		inner, ok := value.Data.(vmValue)
		if !ok {
			return vmValue{}, errors.New("invalid Any value")
		}
		value = inner
	} else if (value.Type.Ref.Kind == types.Interface || value.Type.Ref.Kind == types.Named) && m.isInterfaceType(value.Type) {
		if value.Data == nil {
			return vmValue{}, fmt.Errorf("type assertion failed: nil interface is not %s", targetText)
		}
		inner, ok := value.Data.(vmValue)
		if !ok {
			return vmValue{}, fmt.Errorf("invalid interface value: type=%s data=%T", value.Type, value.Data)
		}
		value = inner
	}
	if _, ok := value.Data.(functionRef); ok && m.isFunctionType(targetText) {
		if !m.functionValueAssignable(value, targetText, &targetVariadic) {
			return vmValue{}, fmt.Errorf("type assertion failed: %s is not %s", value.Type, targetText)
		}
		value.Type = targetType
		return value, nil
	}
	if !m.assignableType(value.Type, targetType) {
		return vmValue{}, fmt.Errorf("type assertion failed: %s is not %s", value.Type, targetText)
	}
	value.Type = targetType
	return value, nil
}

func (m *moduleInstance) typeAssertionValueWithVariadic(value vmValue, target any, targetVariadic bool) (vmValue, error) {
	out, ok, err := m.typeAssertionOKWithVariadic(value, target, targetVariadic)
	if err != nil {
		return vmValue{}, err
	}
	if !ok {
		return vmValue{}, fmt.Errorf("type assertion failed: %s is not %s", value.Type, m.resolvedRuntimeType(target))
	}
	return out, nil
}

func (m *moduleInstance) typeAssertOKValueWithVariadic(value vmValue, target any, targetVariadic bool) (vmValue, vmValue, error) {
	out, ok, err := m.typeAssertionOKWithVariadic(value, target, targetVariadic)
	if err != nil {
		return vmValue{}, vmValue{}, err
	}
	return out, newVMValue("Bool", ok), nil
}

func (m *moduleInstance) typeAssertionOKWithVariadic(value vmValue, target any, targetVariadic bool) (vmValue, bool, error) {
	targetType := m.resolvedRuntimeType(target)
	targetText := targetType.String()
	if targetText == "" {
		return vmValue{}, false, errors.New("missing target type")
	}
	if isNilDynamicInterface(m, value) {
		return m.zeroValue(targetType), false, nil
	}
	if targetType.Ref.Kind == types.Any {
		if value.Type.Ref.Kind == types.Any {
			return value, true, nil
		}
		if m.isInterfaceType(value.Type) {
			inner, ok := value.Data.(vmValue)
			if !ok {
				return vmValue{}, false, fmt.Errorf("invalid interface value: type=%s data=%T", value.Type, value.Data)
			}
			return newVMValue(targetType, inner), true, nil
		}
		return newVMValue(targetType, value), true, nil
	}
	if _, interfaceTarget := targetType.InterfaceInfo(); interfaceTarget {
		var ok bool
		var err error
		value, ok, err = m.unwrapDynamicInterfaceValue(value, targetText)
		if err != nil {
			return vmValue{}, false, err
		}
		if !ok {
			return vmValue{Type: targetType}, true, nil
		}
		if m.implementsInterface(value.Type, targetText) {
			return newVMValue(targetType, value), true, nil
		}
		return m.zeroValue(targetType), false, nil
	}
	if value.Type.Ref.Kind == types.Any {
		inner, ok := value.Data.(vmValue)
		if !ok {
			return vmValue{}, false, errors.New("invalid Any value")
		}
		value = inner
	} else if m.isInterfaceType(value.Type) {
		inner, ok := value.Data.(vmValue)
		if !ok {
			return vmValue{}, false, fmt.Errorf("invalid interface value: type=%s data=%T", value.Type, value.Data)
		}
		value = inner
	}
	if _, ok := value.Data.(functionRef); ok && m.isFunctionType(targetText) {
		if !m.functionValueAssignable(value, targetText, &targetVariadic) {
			return m.zeroValue(targetType), false, nil
		}
		value.Type = targetType
		return value, true, nil
	}
	if !m.sameRuntimeType(value.Type, targetType) {
		return m.zeroValue(targetType), false, nil
	}
	value.Type = targetType
	return value, true, nil
}

func (m *moduleInstance) assignableType(source, target any) bool {
	sourceType := m.resolvedRuntimeType(source)
	targetType := m.resolvedRuntimeType(target)
	if !sourceType.Valid() || !targetType.Valid() {
		return false
	}
	if targetType.Ref.Kind == types.Any {
		return true
	}
	if sourceType.Equal(targetType) || m.sameRuntimeType(sourceType, targetType) {
		return true
	}
	if m.isInterfaceType(targetType) {
		return m.implementsInterface(sourceType, targetType.String())
	}
	if m.isInterfaceType(sourceType) {
		return false
	}
	if m.waitableAssignableType(sourceType, targetType) {
		return true
	}
	sourceUnderlying := m.underlyingRuntimeType(sourceType)
	targetUnderlying := m.underlyingRuntimeType(targetType)
	if sourceUnderlying == "" || targetUnderlying == "" || sourceUnderlying != targetUnderlying {
		return false
	}
	return sourceType.Ref.Kind != types.Named || targetType.Ref.Kind != types.Named
}

func (m *moduleInstance) convertibleType(source, target any) bool {
	sourceType := m.resolvedRuntimeType(source)
	targetType := m.resolvedRuntimeType(target)
	if !sourceType.Valid() || !targetType.Valid() {
		return false
	}
	if m.assignableType(sourceType, targetType) {
		return true
	}
	_, err := m.convertValue(m.zeroValue(sourceType), targetType.String())
	return err == nil
}

func (m *moduleInstance) isFunctionType(typ any) bool {
	_, _, ok := m.functionTypeInfo(typ)
	return ok
}

func (m *moduleInstance) functionTypeInfo(typ any) (string, bool, bool) {
	typeText := strings.TrimSpace(m.resolvedRuntimeType(typ).String())
	if typeText == "" {
		return "", false, false
	}
	if owner, name, ok := m.namedRuntimeType(typeText); ok {
		if owner == nil || owner.executable == nil {
			return "", false, false
		}
		decl, ok := owner.executable.Types[name]
		if !ok {
			return "", false, false
		}
		signature, variadic, ok := owner.functionTypeInfo(owner.formatType(decl.Underlying))
		if !ok {
			return "", false, false
		}
		return signature, variadic, true
	}
	runtimeType := m.resolvedRuntimeType(typeText)
	info, ok := runtimeType.FunctionInfo()
	if !ok {
		return "", false, false
	}
	signatureInfo := info.Signature
	variadic := signatureInfo.Variadic
	signatureInfo.Variadic = false
	signature := types.FormatSignature(runtimeType.Table, signatureInfo)
	return m.qualifyLocalType(signature), variadic, true
}

func (m *moduleInstance) functionValueAssignable(value vmValue, target any, explicitVariadic ...*bool) bool {
	ref, ok := value.Data.(functionRef)
	if !ok || m == nil || m.executable == nil {
		return false
	}
	owner := m
	if ref.exact != nil {
		owner = ref.exact
	} else if modulePath := strings.TrimSpace(ref.ModulePath); modulePath != "" && modulePath != m.modulePath() {
		if m.registry == nil {
			return false
		}
		var found bool
		owner, found = m.registry.module(modulePath)
		if !found {
			return false
		}
	}
	if owner == nil || owner.executable == nil {
		return false
	}
	function, ok := owner.executable.Functions[ref.FunctionID]
	if !ok {
		return false
	}
	sourceSignature := types.FormatSignature(&owner.executable.Artifact.TypeTable, function.Decl.Signature)
	sourceSignature, _, sourceOK := owner.functionTypeInfo(sourceSignature)
	if !sourceOK {
		return false
	}
	targetSignature, targetVariadic, ok := m.functionTypeInfo(target)
	if !ok || sourceSignature != targetSignature {
		return false
	}
	targetText := m.resolvedRuntimeType(target).String()
	if _, _, named := m.namedRuntimeType(targetText); !named {
		if !targetVariadic && len(explicitVariadic) != 0 && explicitVariadic[0] != nil {
			targetVariadic = *explicitVariadic[0]
		}
	}
	return function.Decl.Signature.Variadic == targetVariadic
}

func (m *moduleInstance) waitableAssignableType(source, target any) bool {
	sourceChannel, sourceOK := m.waitableTypeInfo(m.underlyingRuntimeType(source))
	targetChannel, targetOK := m.waitableTypeInfo(m.underlyingRuntimeType(target))
	if !sourceOK || !targetOK {
		return false
	}
	if sourceChannel.direction != types.ChannelBoth {
		return false
	}
	if !m.sameRuntimeType(sourceChannel.elem, targetChannel.elem) {
		return false
	}
	return m.resolvedRuntimeType(source).Ref.Kind != types.Named || m.resolvedRuntimeType(target).Ref.Kind != types.Named
}

type waitableTypeInfo struct {
	direction types.ChannelDir
	elem      vmType
}

func (m *moduleInstance) waitableTypeInfo(typ any) (waitableTypeInfo, bool) {
	info, ok := m.resolvedRuntimeType(typ).WaitableInfo()
	if !ok || strings.TrimSpace(info.Elem.String()) == "" {
		return waitableTypeInfo{}, false
	}
	return waitableTypeInfo{direction: info.Direction, elem: info.Elem}, true
}

func (m *moduleInstance) sameRuntimeType(left, right any) bool {
	leftType := m.resolvedRuntimeType(left)
	rightType := m.resolvedRuntimeType(right)
	if leftType.Equal(rightType) {
		return true
	}
	return m.runtimeTypeIdentity(leftType) == m.runtimeTypeIdentity(rightType)
}

func (m *moduleInstance) runtimeTypeIdentity(typ any) string {
	resolvedType := m.resolvedRuntimeType(typ)
	typeText := strings.TrimSpace(resolvedType.String())
	if typeText == "" {
		return ""
	}
	if m != nil {
		revision := uint64(0)
		if m.registry != nil {
			revision = m.registry.revision
		}
		if cached, ok := m.typeIdentityCache.load(typeText); ok && cached.revision == revision {
			return cached.text
		}
		var identity string
		if module, name, ok := m.namedRuntimeType(typeText); ok {
			identity = qualifiedTypeName(module.modulePath(), name)
		} else {
			identity = m.qualifyLocalType(typeText)
		}
		// Alias declarations can occur inside pointers, slices, and signatures,
		// not just at the outermost type. Resolve their named leaves using the
		// originating table before caching the qualified identity.
		if resolvedType.Table != nil && m.executable != nil {
			if normalized, ok := types.RewriteCanonicalText(identity, func(name string) (string, bool) {
				ref := runtimeTypeFromText(name).Ref
				if ref.Kind != types.Named {
					return name, true
				}
				for _, table := range []*types.TypeTable{resolvedType.Table, &m.executable.Artifact.TypeTable} {
					target := types.NewRelations(table).ResolveAlias(ref)
					if target != ref {
						return types.FormatWithTable(table, target), true
					}
				}
				if m.registry != nil {
					if owner, ok := m.registry.module(ref.Named.ModulePath); ok {
						if export, ok := owner.executable.Exports[string(ref.Named.DeclID)]; ok && export.Kind == "type" {
							return types.FormatWithTable(&owner.executable.Artifact.TypeTable, export.Type), true
						}
					}
				}
				return name, true
			}); ok {
				identity = normalized
			}
		}

		m.typeIdentityCache.store(typeText, typeTextResolution{text: identity, revision: revision, found: true})
		return identity
	}
	if module, name, ok := m.namedRuntimeType(typeText); ok {
		return qualifiedTypeName(module.modulePath(), name)
	}
	return typeText
}

func (m *moduleInstance) underlyingRuntimeType(typ any) string {
	typeText := strings.TrimSpace(m.resolvedRuntimeType(typ).String())
	if typeText == "" {
		return ""
	}
	if module, name, ok := m.namedRuntimeType(typeText); ok {
		return module.underlyingRuntimeTypeInModule(name)
	}
	if m != nil {
		return m.qualifyLocalType(typeText)
	}
	return typeText
}

func (m *moduleInstance) underlyingRuntimeTypeInModule(name string) string {
	if m == nil || m.executable == nil {
		return strings.TrimSpace(name)
	}
	current := strings.TrimSpace(name)
	seen := map[string]struct{}{}
	for i := 0; i < 32; i++ {
		if _, recursive := seen[current]; recursive {
			return m.qualifyLocalType(current)
		}
		seen[current] = struct{}{}
		decl, ok := m.executable.Types[current]
		if !ok {
			return m.qualifyLocalType(current)
		}
		next := strings.TrimSpace(m.formatType(decl.Underlying))
		if next == "" || next == current {
			return m.qualifyLocalType(current)
		}
		if target, targetName, ok := m.qualifiedTypeModule(next); ok {
			if target == m && targetName == current {
				return m.qualifyLocalType(current)
			}
			return target.underlyingRuntimeTypeInModule(targetName)
		}
		if _, ok := m.executable.Types[next]; ok {
			current = next
			continue
		}
		return m.qualifyLocalType(next)
	}
	return m.qualifyLocalType(current)
}

func (m *moduleInstance) namedRuntimeType(typ string) (*moduleInstance, string, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" || m == nil {
		return nil, "", false
	}
	if m.executable != nil {
		if _, ok := m.executable.Types[typ]; ok {
			return m, typ, true
		}
	}
	return m.qualifiedTypeModule(typ)
}

func (m *moduleInstance) modulePath() string {
	if m == nil || m.executable == nil {
		return ""
	}
	return strings.TrimSpace(m.executable.Artifact.Module.Path)
}

func isNilDynamicInterface(m *moduleInstance, value vmValue) bool {
	if value.Type.Ref.Kind == types.Any {
		return value.Data == nil
	}
	return m.isInterfaceType(value.Type) && value.Data == nil
}
