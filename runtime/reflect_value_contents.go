package runtime

import (
	"fmt"
	"strings"
)

func reflectValueMapEntries(ctx intrinsicContext, module *moduleInstance, value vmValue) ([]vmValue, error) {
	if module == nil {
		return nil, nil
	}
	_, _, ok := module.mapKeyValueTypes(value.Type)
	if !ok {
		return nil, nil
	}
	var entries []vmMapEntry
	switch data := value.Data.(type) {
	case *vmMap:
		snapshot := data.snapshot()
		keys := sortedVMMapKeys(snapshot)
		entries = make([]vmMapEntry, 0, len(keys))
		for _, encodedKey := range keys {
			entries = append(entries, snapshot[encodedKey])
		}
	case nil:
		return nil, nil
	default:
		return nil, nil
	}
	out := make([]vmValue, 0, len(entries))
	for i, entry := range entries {
		keySnapshot, err := reflectValueSnapshot(ctx, entry.Key)
		if err != nil {
			return nil, fmt.Errorf("map key %d: %w", i, err)
		}
		valueSnapshot, err := reflectValueSnapshot(ctx, entry.Value)
		if err != nil {
			return nil, fmt.Errorf("map value %d: %w", i, err)
		}
		out = append(out, newRuntimeStructValue(module, "reflect.valueMapEntry", map[string]vmValue{
			"Key":   keySnapshot,
			"Value": valueSnapshot,
		}))
	}
	return out, nil
}

func reflectValueLen(module *moduleInstance, value vmValue) int64 {
	switch data := value.Data.(type) {
	case string:
		return int64(len(data))
	case *vmSlice:
		if data == nil {
			return 0
		}
		return int64(data.Len)
	case *vmArray:
		return int64(data.Len)
	case *vmStruct:
		if module != nil {
			if strings.TrimSpace(reflectTypeInfoForValue(module, value).Kind) == "struct" {
				return int64(len(reflectValueStructFieldNames(module, value)))
			}
		}
	case *vmMap:
		if data == nil {
			return 0
		}
		return int64(data.length())
	}
	return 0
}

func reflectValueCap(module *moduleInstance, value vmValue) int64 {
	switch data := value.Data.(type) {
	case *vmSlice:
		if data == nil {
			return 0
		}
		return int64(data.Cap)
	case *vmArray:
		if module != nil && module.isArrayType(value.Type) && !module.isSliceType(value.Type) {
			return int64(data.Len)
		}
	}
	return 0
}

func reflectValueStructFieldNames(module *moduleInstance, value vmValue) []string {
	info := reflectTypeInfoForValue(module, value)
	if len(info.Fields) != 0 {
		out := make([]string, 0, len(info.Fields))
		for _, field := range info.Fields {
			out = append(out, strings.TrimSpace(field.Name))
		}
		return out
	}
	data, ok := value.Data.(*vmStruct)
	if !ok || data == nil || data.schema == nil {
		return nil
	}
	out := make([]string, 0, len(data.schema.fields))
	for _, field := range data.schema.fields {
		out = append(out, field.Name)
	}
	return out
}
