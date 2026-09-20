package runtime

import (
	"strconv"

	"github.com/d7z-team/mini-go/compiler/types"
)

func zeroVMValue(typ string) vmValue {
	switch typ {
	case "", "Void":
		return newVMValue(typ, nil)
	case "Any":
		return newVMValue(typ, nil)
	case "Bool":
		return newVMValue(typ, false)
	case "String":
		return newVMValue(typ, "")
	case "Function":
		return newVMValue(typ, nil)
	default:
		if value, ok := zeroNumericValue(typ); ok {
			return value
		}
		runtimeType := coerceRuntimeType(typ)
		switch runtimeType.ShapeKind() {
		case types.Function:
			return newVMValue(typ, nil)
		case types.Pointer:
			return newVMValue(typ, nil)
		case types.Slice:
			return newVMValue(typ, (*vmSlice)(nil))
		case types.Map:
			return newVMValue(typ, nil)
		case types.Array:
			length, elem, ok := runtimeType.ArrayInfo()
			if !ok || length > uint64(1<<strconv.IntSize-1) {
				return newVMValue(typ, int64(0))
			}
			values := make([]vmValue, int(length))
			for i := range values {
				values[i] = zeroVMValue(elem.String())
			}
			return newVMValue(typ, values)
		case types.Waitable:
			return newVMValue(typ, nil)
		case types.Struct:
			schema := standaloneStructSchema(runtimeType)
			return newVMValue(runtimeType, &vmStruct{schema: schema})
		}
		return newVMValue(typ, int64(0))
	}
}

func (m *moduleInstance) zeroValue(value any) vmValue {
	runtimeType := m.resolvedRuntimeType(value)
	if m != nil {
		if zero, ok := m.zeroValueCache.load(runtimeType.Ref); ok {
			return zero
		}
	}
	if zero, ok := atomicZeroValue(runtimeType); ok {
		if m != nil {
			m.zeroValueCache.store(runtimeType.Ref, zero)
		}
		return zero
	}
	return m.zeroValueSeen(runtimeType, nil)
}

func (m *moduleInstance) zeroValueSeen(runtimeType vmType, seen map[string]struct{}) vmValue {
	typeText := runtimeType.String()
	if typeText == "" {
		return zeroVMValue(typeText)
	}
	if _, ok := runtimeType.InterfaceInfo(); ok {
		return newVMValue(runtimeType, nil)
	}
	if schema, ok := m.structSchema(runtimeType); ok {
		return newVMValue(runtimeType, &vmStruct{schema: schema})
	}
	if length, elem, ok := runtimeType.ArrayInfo(); ok && length <= uint64(1<<strconv.IntSize-1) {
		values := make([]vmValue, int(length))
		for i := range values {
			values[i] = m.zeroValueSeen(elem, seen)
		}
		return newVMValue(runtimeType, values)
	}
	if zero, ok := atomicZeroValue(runtimeType); ok {
		return zero
	}
	if m != nil {
		if owner, name, ok := m.qualifiedTypeModule(typeText); ok && owner != nil && owner.executable != nil {
			decl, ok := owner.executable.Types[name]
			if ok && decl.Underlying.Valid() {
				key := owner.modulePath() + "." + name
				if _, recursive := seen[key]; recursive {
					return newVMValue(runtimeType, nil)
				}
				if seen == nil {
					seen = make(map[string]struct{})
				}
				seen[key] = struct{}{}
				value := owner.zeroNamedTypeValue(decl, seen)
				delete(seen, key)
				return owner.qualifyValueForExport(value)
			}
		}
	}
	if m != nil && m.executable != nil {
		if decl, ok := m.executable.Types[typeText]; ok && decl.Underlying.Valid() {
			key := m.modulePath() + "." + typeText
			if _, recursive := seen[key]; recursive {
				return newVMValue(runtimeType, nil)
			}
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[key] = struct{}{}
			value := m.zeroNamedTypeValue(decl, seen)
			delete(seen, key)
			return value
		}
	}
	value := zeroVMValue(typeText)
	value.Type = runtimeType
	return value
}

func atomicZeroValue(runtimeType vmType) (vmValue, bool) {
	switch runtimeType.ShapeKind() {
	case types.Void, types.Any, types.Function, types.Pointer, types.Map, types.Waitable, types.Interface:
		return newVMValue(runtimeType, nil), true
	case types.Slice:
		return newVMValue(runtimeType, (*vmSlice)(nil)), true
	case types.Primitive:
		if zero, ok := zeroNumericValue(runtimeType); ok {
			return zero, true
		}
		underlying := runtimeType.Underlying().Ref
		if underlying.Kind != types.Primitive {
			return vmValue{}, false
		}
		switch underlying.Primitive {
		case types.PrimitiveBool:
			return newVMValue(runtimeType, false), true
		case types.PrimitiveString:
			return newVMValue(runtimeType, ""), true
		case types.PrimitiveError, types.PrimitiveFunction:
			return newVMValue(runtimeType, nil), true
		}
	}
	return vmValue{}, false
}

func (m *moduleInstance) zeroNamedTypeValue(decl types.TypeNode, seen map[string]struct{}) vmValue {
	ref := m.runtimeType(types.Ref(decl))
	if schema, ok := m.structSchema(ref); ok {
		return newVMValue(ref, &vmStruct{schema: schema})
	}
	value := m.zeroValueSeen(m.runtimeType(decl.Underlying), seen)
	value.Type = ref
	return value
}
