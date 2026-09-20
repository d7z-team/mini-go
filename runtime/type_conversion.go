package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func convertToString(value vmValue) (vmValue, error) {
	if out, ok := value.Data.(string); ok {
		return newVMValue("String", out), nil
	}
	if !isIntegerValue(value) {
		return vmValue{}, fmt.Errorf("cannot convert %s to String", value.Type)
	}
	n, err := numericAsInt64(value)
	if err != nil {
		return vmValue{}, err
	}
	return newVMValue("String", string(rune(n))), nil
}

func convertToIntegerLike(value vmValue, target vmType) (vmValue, error) {
	if !isNumericValue(value) {
		return vmValue{}, fmt.Errorf("cannot convert %s to %s", value.Type, target)
	}
	info, ok := target.NumericInfo()
	if !ok {
		return vmValue{}, fmt.Errorf("unsupported integer target %s", target)
	}
	if info.Kind == types.NumericUnsigned {
		n, err := numericAsUint64(value)
		if err != nil {
			return vmValue{}, err
		}
		return normalizeUnsignedValue(target, n), nil
	}
	if info.Kind == types.NumericSigned {
		n, err := numericAsInt64(value)
		if err != nil {
			return vmValue{}, err
		}
		return normalizeSignedValue(target, n), nil
	}
	return vmValue{}, fmt.Errorf("unsupported integer target %s", target)
}

func (m *moduleInstance) convertValue(value vmValue, target any) (vmValue, error) {
	return m.convertValueWithVariadic(value, target, false)
}

func (m *moduleInstance) convertValueWithVariadic(value vmValue, target any, targetVariadic bool) (vmValue, error) {
	targetType := m.resolvedRuntimeType(target)
	targetText := targetType.String()
	if targetText == "" {
		return vmValue{}, errors.New("missing target type")
	}
	if targetType.Ref.Kind == types.Any {
		if value.Type.Ref.Kind == types.Any {
			return value, nil
		}
		return newVMValue(targetType, value), nil
	}
	if _, interfaceTarget := targetType.InterfaceInfo(); interfaceTarget {
		out, ok := m.assertInterfaceValue(value, targetText)
		if !ok {
			return vmValue{}, fmt.Errorf("cannot convert %s to %s: missing interface methods", value.Type, targetText)
		}
		out.Type = targetType
		return out, nil
	}
	if value.Type.Ref.Kind == types.Any {
		return vmValue{}, fmt.Errorf("cannot convert Any to %s; use a type assertion", targetText)
	} else if m.isInterfaceType(value.Type) {
		return vmValue{}, fmt.Errorf("cannot convert %s to %s; use a type assertion", value.Type, targetText)
	}
	if value.Type.Equal(targetType) {
		return value, nil
	}
	if m.sameRuntimeType(value.Type, targetType) {
		value.Type = targetType
		return value, nil
	}
	if sourceElem, sourcePointer := value.Type.PointerElem(); sourcePointer && value.Type.Ref.Kind != types.Named && targetType.Ref.Kind != types.Named {
		if targetElem, targetPointer := targetType.PointerElem(); targetPointer &&
			m.conversionUnderlyingIdentical(sourceElem, targetElem) {
			pointer, ok := value.Data.(*vmPointer)
			if !ok && value.Data != nil {
				return vmValue{}, fmt.Errorf("invalid pointer payload for %s", value.Type)
			}
			if pointer == nil {
				return newVMValue(targetType, (*vmPointer)(nil)), nil
			}
			// A typed view always points directly at the original storage.
			if pointer.original != nil {
				pointer = pointer.original
			}
			if m.sameRuntimeType(pointer.Type, targetElem) {
				return newVMValue(targetType, pointer), nil
			}
			return newVMValue(targetType, &vmPointer{Type: targetElem, Identity: pointer.Identity, original: pointer}), nil
		}
	}
	if sourceWaitable, sourceOK := value.Type.WaitableInfo(); sourceOK {
		if targetWaitable, targetOK := targetType.WaitableInfo(); targetOK {
			if !m.sameRuntimeType(sourceWaitable.Elem, targetWaitable.Elem) {
				return vmValue{}, fmt.Errorf("cannot convert %s to %s: channel element types differ", value.Type, targetText)
			}
			if sourceWaitable.Direction != types.ChannelBoth && sourceWaitable.Direction != targetWaitable.Direction {
				return vmValue{}, fmt.Errorf("cannot convert %s to %s: channel directions are incompatible", value.Type, targetText)
			}
			value.Type = targetType
			return value, nil
		}
	}
	if _, ok := value.Data.(functionRef); ok && m.isFunctionType(targetText) {
		if !m.functionValueAssignable(value, targetText, &targetVariadic) {
			return vmValue{}, fmt.Errorf("cannot convert %s to %s: function signatures differ", value.Type, targetText)
		}
		value.Type = targetType
		return value, nil
	}
	if out, ok, err := m.convertSliceToArrayPointer(value, targetType); ok || err != nil {
		return out, err
	}
	if out, ok, err := m.convertSliceToArray(value, targetType); ok || err != nil {
		return out, err
	}
	if out, ok := m.convertSameUnderlyingComposite(value, targetText); ok {
		out.Type = targetType
		return out, nil
	}
	if m.isByteSliceType(targetText) {
		return convertStringToByteSlice(value, targetText)
	}
	if m.isRuneSliceType(targetText) {
		return convertStringToRuneSlice(value, targetText)
	}
	if targetType.Primitive(types.PrimitiveString) {
		if out, ok, err := m.convertByteSliceToString(value); ok || err != nil {
			return out, err
		}
		if out, ok, err := m.convertRuneSliceToString(value); ok || err != nil {
			return out, err
		}
	}
	if underlying, ok := m.underlyingType(targetText); ok {
		underlyingType := m.resolvedRuntimeType(underlying)
		_, numeric := underlyingType.NumericInfo()
		if !underlyingType.Primitive(types.PrimitiveBool) && !underlyingType.Primitive(types.PrimitiveString) && !numeric {
			return vmValue{}, fmt.Errorf("unsupported conversion from %s to %s", value.Type, targetText)
		}
		out, err := m.convertValue(value, underlying)
		if err != nil {
			return vmValue{}, err
		}
		out.Type = targetType
		return out, nil
	}
	switch targetText {
	case "Bool":
		out, ok := value.Data.(bool)
		if !ok {
			return vmValue{}, fmt.Errorf("cannot convert %s to Bool", value.Type)
		}
		return newBoolValue(out), nil
	case "String":
		return convertToString(value)
	case "Float32", "Float64":
		out, err := numericAsFloat64(value)
		if err != nil {
			return vmValue{}, fmt.Errorf("cannot convert %s to %s: %w", value.Type, targetText, err)
		}
		return normalizeFloatValue(targetType, out), nil
	case "Complex64", "Complex128":
		out, err := numericAsComplex128(value)
		if err != nil {
			return vmValue{}, fmt.Errorf("cannot convert %s to %s: %w", value.Type, targetText, err)
		}
		return normalizeComplexValue(targetType, out), nil
	default:
		if _, numeric := targetType.NumericInfo(); numeric {
			return convertToIntegerLike(value, targetType)
		}
		return vmValue{}, fmt.Errorf("unsupported conversion from %s to %s", value.Type, targetText)
	}
}

func (m *moduleInstance) convertSliceToArrayPointer(value vmValue, target vmType) (vmValue, bool, error) {
	pointee, ok := target.PointerElem()
	if !ok {
		return vmValue{}, false, nil
	}
	arrayRuntimeType := m.resolvedRuntimeType(pointee)
	length, elem, ok := arrayRuntimeType.ArrayInfo()
	if !ok || length > uint64(1<<strconv.IntSize-1) {
		return vmValue{}, false, nil
	}
	if !m.isSliceType(value.Type) {
		return vmValue{}, false, nil
	}
	sourceElem := m.arrayElemType(value.Type)
	elemType := elem.String()
	if !m.sameRuntimeType(sourceElem, elemType) {
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: element types differ", value.Type, target)
	}
	slice, ok := value.Data.(*vmSlice)
	if !ok {
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: invalid slice backing", value.Type, target)
	}
	n := int(length)
	if slice == nil && n == 0 {
		return newVMValue(target, (*vmPointer)(nil)), true, nil
	}
	if slice == nil || slice.Len < n {
		currentLen := 0
		if slice != nil {
			currentLen = slice.Len
		}
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: slice length %d is smaller than array length %d", value.Type, target, currentLen, n)
	}
	backingLen := len(slice.Backing)
	if slice.ByteBacked {
		backingLen = len(slice.ByteBacking)
	}
	if slice.Start < 0 || slice.Start+n > backingLen {
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: invalid slice backing", value.Type, target)
	}
	start := slice.Start
	identity := fmt.Sprintf("slice-array:%p:%d", slice.Backing, start)
	if slice.ByteBacked {
		identity = fmt.Sprintf("slice-array:%p:%d", slice.ByteBacking, start)
	}
	pointer := newTargetPointer(&vmPointer{Type: arrayRuntimeType, Identity: identity, target: pointerArray, module: m, array: slice, arrayLen: n})
	pointer.Type = target
	return pointer, true, nil
}

func (m *moduleInstance) convertSliceToArray(value vmValue, target vmType) (vmValue, bool, error) {
	length, elem, ok := target.ArrayInfo()
	if !ok || length > uint64(1<<strconv.IntSize-1) {
		return vmValue{}, false, nil
	}
	if !m.isSliceType(value.Type) {
		return vmValue{}, false, nil
	}
	sourceElem := m.arrayElemType(value.Type)
	elemType := elem.String()
	if !m.sameRuntimeType(sourceElem, elemType) {
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: element types differ", value.Type, target)
	}
	values, ok := sliceValues(value)
	if !ok {
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: invalid slice backing", value.Type, target)
	}
	n := int(length)
	if len(values) < n {
		return vmValue{}, true, fmt.Errorf("cannot convert %s to %s: slice length %d is smaller than array length %d", value.Type, target, len(values), n)
	}
	out := make([]vmValue, n)
	for i := range out {
		normalized, err := m.coerceAssignableValue(values[i], elemType)
		if err != nil {
			return vmValue{}, true, fmt.Errorf("array element %d: %w", i, err)
		}
		out[i] = m.cloneValueForStore(normalized)
	}
	return newVMValue(target, out), true, nil
}

func (m *moduleInstance) convertSameUnderlyingComposite(value vmValue, target string) (vmValue, bool) {
	targetUnderlying := m.underlyingRuntimeType(target)
	if !m.conversionUnderlyingIdentical(value.Type, target) {
		return vmValue{}, false
	}
	switch m.resolvedRuntimeType(targetUnderlying).ShapeKind() {
	case types.Slice, types.Array, types.Map, types.Waitable, types.Struct:
		out := m.cloneValueForStore(value)
		out.Type = coerceRuntimeType(target)
		return out, true
	default:
		return vmValue{}, false
	}
}

func (m *moduleInstance) conversionUnderlyingIdentical(source, target any) bool {
	left := m.underlyingRuntimeType(source)
	right := m.underlyingRuntimeType(target)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	table := types.NewTable()
	parser := types.NewParser("", table)
	leftRef, leftErr := parser.Parse(left)
	rightRef, rightErr := parser.Parse(right)
	return leftErr == nil && rightErr == nil && types.NewRelations(table).UnderlyingIdenticalIgnoringTags(leftRef, rightRef).OK
}

func (m *moduleInstance) underlyingType(typ string) (string, bool) {
	if m == nil || m.executable == nil {
		return "", false
	}
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return "", false
	}
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	if cached, ok := m.underlyingTypeCache.load(typ); ok && cached.revision == revision {
		return cached.text, cached.found
	}
	original := typ
	seen := map[string]struct{}{}
	for i := 0; i < 32; i++ {
		if _, ok := seen[typ]; ok {
			break
		}
		seen[typ] = struct{}{}
		if module, name, ok := m.qualifiedTypeModule(typ); ok {
			decl, ok := module.executable.Types[name]
			if !ok {
				break
			}
			next := strings.TrimSpace(module.qualifyLocalType(module.formatType(decl.Underlying)))
			if next == "" || next == typ {
				break
			}
			typ = next
			continue
		}
		decl, ok := m.executable.Types[typ]
		if !ok {
			break
		}
		next := strings.TrimSpace(m.formatType(decl.Underlying))
		if next == "" || next == typ {
			break
		}
		typ = next
	}

	found := typ != original
	m.underlyingTypeCache.store(original, typeTextResolution{text: typ, revision: revision, found: found})
	return typ, found
}
