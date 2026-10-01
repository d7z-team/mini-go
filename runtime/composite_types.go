package runtime

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) sliceValues(value vmValue) ([]vmValue, bool) {
	if !m.isSliceType(value.Type) {
		return nil, false
	}
	return sliceValues(value)
}

func (m *moduleInstance) sequenceElementType(typ any) string {
	runtimeType := m.resolvedRuntimeType(typ)
	if elem, ok := runtimeType.SliceElem(); ok {
		return elem.String()
	}
	if _, elem, ok := runtimeType.ArrayInfo(); ok {
		return elem.String()
	}
	if runtimeType.Primitive(types.PrimitiveString) {
		return "Uint8"
	}
	return "Any"
}

func (m *moduleInstance) isArrayType(typ any) bool {
	return m.resolvedRuntimeType(typ).ShapeKind() == types.Array
}

func (m *moduleInstance) isSliceType(typ any) bool {
	return m.resolvedRuntimeType(typ).ShapeKind() == types.Slice
}

func (m *moduleInstance) arrayType(typ any) (int64, string, bool) {
	length, elem, ok := m.resolvedRuntimeType(typ).ArrayInfo()
	if !ok || length > uint64(1<<strconv.IntSize-1) {
		return 0, "", false
	}
	return int64(length), elem.String(), true
}

func (m *moduleInstance) sliceResultType(typ any) string {
	if _, elem, ok := m.arrayType(typ); ok {
		return "Slice<" + elem + ">"
	}
	return m.resolvedRuntimeType(typ).String()
}

func (m *moduleInstance) mapKeyValueTypes(typ any) (string, string, bool) {
	key, value, ok := m.resolvedRuntimeType(typ).MapInfo()
	if !ok {
		return "", "", false
	}
	keyType := strings.TrimSpace(key.String())
	valueType := strings.TrimSpace(value.String())
	if keyType == "" || valueType == "" {
		return "", "", false
	}
	return keyType, valueType, true
}

func (m *moduleInstance) isMapType(typ any) bool {
	_, _, ok := m.mapKeyValueTypes(typ)
	return ok
}

func (m *moduleInstance) normalizedMapKey(object, key vmValue) (vmMapKey, error) {
	if keyType, _, ok := m.mapKeyValueTypes(object.Type); ok {
		normalized, err := m.coerceAssignableValue(key, keyType)
		if err != nil {
			return vmMapKey{}, fmt.Errorf("map key: %w", err)
		}
		key = normalized
	}
	return m.mapKey(key)
}
