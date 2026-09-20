package runtime

import (
	"context"
	"fmt"
	"math"
	"strings"
)

func hostResult(result vmResult, limits Limits) (RunResult, error) {
	return hostResultContext(context.Background(), result, limits)
}

func hostResultContext(ctx context.Context, result vmResult, limits Limits) (RunResult, error) {
	values := make([]HostValue, len(result.Values))
	for index, value := range result.Values {
		if err := ctx.Err(); err != nil {
			return RunResult{}, err
		}
		converted, err := vmValueToHost(ctx, value, limits, 0)
		if err != nil {
			return RunResult{}, fmt.Errorf("host result %d: %w", index, err)
		}
		if _, err := measureHostValue(ctx, converted, limits); err != nil {
			return RunResult{}, fmt.Errorf("host result %d: %w", index, err)
		}
		values[index] = converted
	}
	return RunResult{Values: values}, nil
}

func hostValueToVM(ctx context.Context, module *moduleInstance, expected string, value HostValue) (vmValue, error) {
	if err := ctx.Err(); err != nil {
		return vmValue{}, err
	}
	if expected == "" {
		expected = value.typ
	}
	if value.typ != expected && expected != "Any" {
		return vmValue{}, fmt.Errorf("host value type mismatch: got %s, want %s", value.typ, expected)
	}
	var out vmValue
	switch value.kind {
	case HostNilKind:
		out = newVMValue(expected, nil)
	case HostBoolKind:
		out = newVMValue(expected, value.boolValue)
	case HostIntKind:
		out = newVMValue(expected, value.intValue)
	case HostUintKind:
		out = newVMValue(expected, value.uintValue)
	case HostFloatKind:
		if expected == "Float32" {
			out = newVMValue(expected, math.Float32frombits(uint32(value.floatBits)))
		} else {
			out = newVMValue(expected, math.Float64frombits(value.floatBits))
		}
	case HostComplexKind:
		if expected == "Complex64" {
			out = newVMValue(expected, complex(math.Float32frombits(uint32(value.realBits)), math.Float32frombits(uint32(value.imagBits))))
		} else {
			out = newVMValue(expected, complex(math.Float64frombits(value.realBits), math.Float64frombits(value.imagBits)))
		}
	case HostStringKind:
		out = newVMValue(expected, value.text)
	case HostBytesKind:
		bytes := append([]byte(nil), value.bytes...)
		out = newByteSliceHeaderValue(expected, bytes, len(bytes), cap(bytes))
	case HostArrayKind, HostSliceKind:
		items := make([]vmValue, len(value.items))
		elementType := module.arrayElemType(expected)
		for i := range value.items {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return vmValue{}, err
				}
			}
			input := value.items[i]
			input.typ = elementType
			item, err := hostValueToVM(ctx, module, elementType, input)
			if err != nil {
				return vmValue{}, fmt.Errorf("host collection item %d: %w", i, err)
			}
			items[i] = item
		}
		if value.kind == HostSliceKind {
			out = newSliceValue(expected, items)
		} else {
			out = newVMValue(expected, items)
		}
	case HostMapKind:
		pairs := make([]vmValue, 0, len(value.entries)*2)
		keyType, valueType, _ := module.mapKeyValueTypes(expected)
		for i := range value.entries {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return vmValue{}, err
				}
			}
			keyInput := value.entries[i].Key
			keyInput.typ = keyType
			key, err := hostValueToVM(ctx, module, keyType, keyInput)
			if err != nil {
				return vmValue{}, fmt.Errorf("host map key %d: %w", i, err)
			}
			valueInput := value.entries[i].Value
			valueInput.typ = valueType
			item, err := hostValueToVM(ctx, module, valueType, valueInput)
			if err != nil {
				return vmValue{}, fmt.Errorf("host map value %d: %w", i, err)
			}
			pairs = append(pairs, key, item)
		}
		var err error
		out, err = newMapValue(module, expected, pairs)
		if err != nil {
			return vmValue{}, err
		}
	case HostStructKind:
		names := make([]string, 0, len(value.fields))
		values := make([]vmValue, 0, len(value.fields))
		fieldTypes, _ := moduleStructFields(module, expected)
		typesByName := make(map[string]string, len(fieldTypes))
		for _, field := range fieldTypes {
			typesByName[field.Name] = field.Type
		}
		for index, field := range value.fields {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return vmValue{}, err
				}
			}
			fieldType := typesByName[field.Name]
			if fieldType == "" {
				fieldType = field.Value.typ
			}
			input := field.Value
			input.typ = fieldType
			item, err := hostValueToVM(ctx, module, fieldType, input)
			if err != nil {
				return vmValue{}, fmt.Errorf("host struct field %s: %w", field.Name, err)
			}
			names = append(names, field.Name)
			values = append(values, item)
		}
		var err error
		out, err = newStructValue(module, expected, names, values)
		if err != nil {
			return vmValue{}, err
		}
	case HostAnyKind:
		inner, err := hostValueToVM(ctx, module, value.dynamic.typ, *value.dynamic)
		if err != nil {
			return vmValue{}, err
		}
		out = newVMValue("Any", inner)
	default:
		return vmValue{}, fmt.Errorf("unsupported host value kind %q", value.kind)
	}
	return module.coerceAssignableValue(out, expected)
}

func moduleStructFields(module *moduleInstance, typ string) ([]structFieldType, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return nil, false
	}
	runtimeType := coerceRuntimeType(typ)
	if module != nil {
		runtimeType = module.resolvedRuntimeType(typ)
	}
	if fields, ok := runtimeStructFieldTypes(runtimeType); ok {
		return fields, true
	}
	if module == nil || module.executable == nil {
		return nil, false
	}
	owner := module
	name := typ
	if resolved, resolvedName, ok := module.qualifiedTypeModule(typ); ok {
		owner = resolved
		name = resolvedName
	}
	if owner == nil || owner.executable == nil {
		return nil, false
	}
	decl, ok := owner.executable.Types[name]
	if !ok {
		return nil, false
	}
	if typeFields := owner.executable.typeFields(decl); len(typeFields) != 0 {
		fields := make([]structFieldType, 0, len(typeFields))
		for _, field := range typeFields {
			name := strings.TrimSpace(field.Name)
			fieldType := strings.TrimSpace(owner.formatType(field.Type))
			if name == "" || name == "_" || fieldType == "" {
				continue
			}
			fields = append(fields, structFieldType{Name: name, Type: fieldType})
		}
		return fields, true
	}
	return runtimeStructFieldTypes(owner.resolvedRuntimeType(owner.formatType(decl.Underlying)))
}

func vmValueToHost(ctx context.Context, value vmValue, limits Limits, depth int) (HostValue, error) {
	if err := ctx.Err(); err != nil {
		return HostValue{}, err
	}
	if depth > limits.MaxBoundaryDepth {
		return HostValue{}, fmt.Errorf("host value depth limit exceeded: max %d", limits.MaxBoundaryDepth)
	}
	var out HostValue
	typ := value.Type.String()
	if data, ok := value.signedValue(); ok {
		return HostInt(typ, data), nil
	}
	if data, ok := value.unsignedValue(); ok {
		return HostUint(typ, data), nil
	}
	if data, ok := value.floatValue(); ok {
		if typ == "Float32" {
			out = HostFloat32(float32(data))
		} else {
			out = HostFloat64(data)
		}
		return out, nil
	}
	switch data := value.Data.(type) {
	case nil:
		out = HostNil(typ)
	case bool:
		out = HostBool(data)
	case int:
		out = HostInt(typ, int64(data))
	case int8:
		out = HostInt(typ, int64(data))
	case int16:
		out = HostInt(typ, int64(data))
	case int32:
		out = HostInt(typ, int64(data))
	case int64:
		out = HostInt(typ, data)
	case uint:
		out = HostUint(typ, uint64(data))
	case uint8:
		out = HostUint(typ, uint64(data))
	case uint16:
		out = HostUint(typ, uint64(data))
	case uint32:
		out = HostUint(typ, uint64(data))
	case uint64:
		out = HostUint(typ, data)
	case float32:
		out = HostFloat32(data)
	case float64:
		if typ == "Float32" {
			out = HostFloat32(float32(data))
		} else {
			out = HostFloat64(data)
		}
	case complex64:
		out = HostComplex64(data)
	case complex128:
		if typ == "Complex64" {
			out = HostComplex64(complex64(data))
		} else {
			out = HostComplex128(data)
		}
	case string:
		out = HostString(data)
	case vmValue:
		inner, err := vmValueToHost(ctx, data, limits, depth+1)
		if err != nil {
			return HostValue{}, err
		}
		out = HostValue{typ: "Any", kind: HostAnyKind, dynamic: &inner}
	case *vmArray:
		values := data.values()
		items := make([]HostValue, len(values))
		for i := range values {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return HostValue{}, err
				}
			}
			item, err := vmValueToHost(ctx, values[i], limits, depth+1)
			if err != nil {
				return HostValue{}, fmt.Errorf("host array item %d: %w", i, err)
			}
			items[i] = item
		}
		out = HostValue{typ: typ, kind: HostArrayKind, items: items}
	case *vmSlice:
		if data == nil {
			out = HostNil(typ)
			break
		}
		items, ok := sliceValues(value)
		if !ok {
			return HostValue{}, fmt.Errorf("invalid slice data %T", value.Data)
		}
		if typ == "Slice<Uint8>" {
			if data.ByteBacked {
				out = HostValue{typ: "Slice<Uint8>", kind: HostBytesKind, bytes: data.bytes()}
				break
			}
			bytes := make([]byte, len(items))
			for i, item := range items {
				if i&255 == 0 {
					if err := ctx.Err(); err != nil {
						return HostValue{}, err
					}
				}
				number, ok := item.unsignedValue()
				if !ok || number > math.MaxUint8 {
					return HostValue{}, fmt.Errorf("host byte slice item %d is not Uint8", i)
				}
				bytes[i] = byte(number)
			}
			out = HostValue{typ: "Slice<Uint8>", kind: HostBytesKind, bytes: bytes}
			break
		}
		values := make([]HostValue, len(items))
		for i := range items {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return HostValue{}, err
				}
			}
			item, err := vmValueToHost(ctx, items[i], limits, depth+1)
			if err != nil {
				return HostValue{}, fmt.Errorf("host slice item %d: %w", i, err)
			}
			values[i] = item
		}
		out = HostValue{typ: typ, kind: HostSliceKind, items: values}
	case *vmMap:
		snapshot := data.snapshot()
		keys := sortedVMMapKeys(snapshot)
		entries := make([]HostMapEntry, 0, len(keys))
		for index, key := range keys {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return HostValue{}, err
				}
			}
			entry := snapshot[key]
			hostKey, err := vmValueToHost(ctx, entry.Key, limits, depth+1)
			if err != nil {
				return HostValue{}, err
			}
			hostValue, err := vmValueToHost(ctx, entry.Value, limits, depth+1)
			if err != nil {
				return HostValue{}, err
			}
			entries = append(entries, HostMapEntry{Key: hostKey, Value: hostValue})
		}
		out = HostValue{typ: typ, kind: HostMapKind, entries: entries}
	case *vmStruct:
		if data == nil || data.schema == nil {
			return HostValue{}, fmt.Errorf("invalid struct value %s", value.Type)
		}
		fields := make([]HostField, 0, len(data.schema.fields))
		for index, info := range data.schema.fields {
			if index&255 == 0 {
				if err := ctx.Err(); err != nil {
					return HostValue{}, err
				}
			}
			field := zeroVMValue(info.RuntimeType.String())
			if stored, ok := data.fieldAt(index); ok {
				field = stored
			}
			hostField, err := vmValueToHost(ctx, field, limits, depth+1)
			if err != nil {
				return HostValue{}, err
			}
			fields = append(fields, HostField{Name: info.Name, Value: hostField})
		}
		out = HostValue{typ: typ, kind: HostStructKind, fields: fields}
	default:
		return HostValue{}, fmt.Errorf("runtime-only %T cannot cross host value boundary", value.Data)
	}
	return out, nil
}
