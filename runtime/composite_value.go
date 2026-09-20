package runtime

import (
	"errors"
	"fmt"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
)

type vmSlice struct {
	*vmSliceStorage
	Start int
	Len   int
	Cap   int
}

type vmSliceStorage struct {
	mu          sync.Mutex
	Backing     []vmValue
	ByteBacking []byte
	ByteBacked  bool
}

type vmArray struct {
	vmSlice
}

func (s *vmSlice) values() []vmValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ByteBacked {
		values := make([]vmValue, s.Len)
		for index := range values {
			values[index] = newVMValue("Uint8", uint64(s.ByteBacking[s.Start+index]))
		}
		return values
	}
	return append([]vmValue(nil), s.Backing[s.Start:s.Start+s.Len]...)
}

func (s *vmSlice) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.ByteBacking[s.Start:s.Start+s.Len]...)
}

func (s *vmSlice) writeBytes(offset int, values []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ByteBacked {
		copy(s.ByteBacking[s.Start+offset:], values)
		return
	}
	for index, value := range values {
		s.Backing[s.Start+offset+index] = newVMValue("Uint8", uint64(value))
	}
}

func (storage *vmSliceStorage) valuesSnapshot() []vmValue {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	return append([]vmValue(nil), storage.Backing...)
}

func newSliceValue(typ any, values []vmValue) vmValue {
	backing := make([]vmValue, len(values))
	copy(backing, values)
	return newSliceHeaderValue(typ, backing, 0, len(backing), cap(backing))
}

func newOwnedSliceValue(typ any, backing []vmValue) vmValue {
	return newSliceHeaderValue(typ, backing, 0, len(backing), cap(backing))
}

func newByteSliceValue(typ any, text string) vmValue {
	backing := []byte(text)
	return newByteSliceHeaderValue(typ, backing, len(backing), cap(backing))
}

func newByteSliceHeaderValue(typ any, backing []byte, length, capacity int) vmValue {
	if capacity > len(backing) && capacity <= cap(backing) {
		backing = backing[:capacity]
	}
	return newVMValue(typ, &vmSlice{
		vmSliceStorage: &vmSliceStorage{ByteBacking: backing, ByteBacked: true},
		Len:            length,
		Cap:            capacity,
	})
}

func (s *vmSlice) valueAt(index int) vmValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ByteBacked {
		return newVMValue("Uint8", uint64(s.ByteBacking[s.Start+index]))
	}
	return s.Backing[s.Start+index]
}

func (s *vmSlice) setValueAt(index int, value vmValue) error {
	if s.ByteBacked {
		n, err := numericAsUint64(value)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.ByteBacking[s.Start+index] = byte(n)
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.Backing[s.Start+index] = value
	s.mu.Unlock()
	return nil
}

func newSliceHeaderValue(typ any, backing []vmValue, start, length, capacity int) vmValue {
	if backing == nil && start == 0 && length == 0 && capacity == 0 {
		return newVMValue(typ, (*vmSlice)(nil))
	}
	if required := start + capacity; required > len(backing) && required <= cap(backing) {
		backing = backing[:required]
	}
	return newVMValue(typ, &vmSlice{
		vmSliceStorage: &vmSliceStorage{Backing: backing},
		Start:          start,
		Len:            length,
		Cap:            capacity,
	})
}

func newSliceViewValue(typ any, source *vmSlice, start, length, capacity int) vmValue {
	if source == nil {
		return newSliceHeaderValue(typ, nil, start, length, capacity)
	}
	return newVMValue(typ, &vmSlice{
		vmSliceStorage: source.vmSliceStorage,
		Start:          start, Len: length, Cap: capacity,
	})
}

func sliceValues(value vmValue) ([]vmValue, bool) {
	switch data := value.Data.(type) {
	case *vmSlice:
		if data == nil {
			return nil, true
		}
		if data.Len == 0 {
			return []vmValue{}, true
		}
		return data.values(), true
	default:
		return nil, false
	}
}

func sliceLen(value vmValue) (int, bool) {
	switch data := value.Data.(type) {
	case *vmSlice:
		if data == nil {
			return 0, true
		}
		return data.Len, true
	default:
		return 0, false
	}
}

func sliceCap(value vmValue) (int, bool) {
	switch data := value.Data.(type) {
	case *vmSlice:
		if data == nil {
			return 0, true
		}
		return data.Cap, true
	default:
		return 0, false
	}
}

func newSequenceValue(module *moduleInstance, typ any, values []vmValue) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	typeText := runtimeType.String()
	isArray := false
	if length, _, ok := module.arrayType(typeText); ok {
		isArray = true
		if int64(len(values)) != length {
			return vmValue{}, fmt.Errorf("array length mismatch: got %d, want %d", len(values), length)
		}
	}
	elemType := module.arrayElemType(typeText)
	out := make([]vmValue, len(values))
	for i, value := range values {
		normalized, err := module.coerceAssignableValue(value, elemType)
		if err != nil {
			return vmValue{}, fmt.Errorf("array element %d: %w", i, err)
		}
		out[i] = module.cloneValueForStore(normalized)
	}
	if !isArray && module.isSliceType(typeText) {
		if module.sameRuntimeType(elemType, "Uint8") {
			bytes := make([]byte, len(out))
			for i, value := range out {
				n, err := numericAsUint64(value)
				if err != nil {
					return vmValue{}, fmt.Errorf("byte slice element %d: %w", i, err)
				}
				bytes[i] = byte(n)
			}
			return newByteSliceHeaderValue(runtimeType, bytes, len(bytes), cap(bytes)), nil
		}
		return newSliceValue(runtimeType, out), nil
	}
	return newVMValue(runtimeType, out), nil
}

func newMapValue(module *moduleInstance, typ any, pairs []vmValue) (vmValue, error) {
	return newMapValueWithCapacity(module, typ, pairs, len(pairs)/2)
}

func newMapValueWithCapacity(module *moduleInstance, typ any, pairs []vmValue, capacity int) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	typeText := runtimeType.String()
	if len(pairs)%2 != 0 {
		return vmValue{}, errors.New("map construction requires key/value pairs")
	}
	if capacity < len(pairs)/2 {
		capacity = len(pairs) / 2
	}
	type preparedMapEntry struct {
		key   vmMapKey
		entry vmMapEntry
	}
	entries := make([]preparedMapEntry, 0, len(pairs)/2)
	keyType, valueType, typedMap := module.mapKeyValueTypes(typeText)
	for i := 0; i < len(pairs); i += 2 {
		keyValue := pairs[i]
		valueToStore := pairs[i+1]
		if typedMap {
			normalizedKey, err := module.coerceAssignableValue(pairs[i], keyType)
			if err != nil {
				return vmValue{}, fmt.Errorf("map key %d: %w", i/2, err)
			}
			keyValue = normalizedKey
			normalizedValue, err := module.coerceAssignableValue(pairs[i+1], valueType)
			if err != nil {
				return vmValue{}, fmt.Errorf("map value %d: %w", i/2, err)
			}
			valueToStore = normalizedValue
		}
		key, err := module.mapKey(keyValue)
		if err != nil {
			return vmValue{}, err
		}
		entries = append(entries, preparedMapEntry{
			key: key, entry: vmMapEntry{Key: module.cloneValueForStore(keyValue), Value: module.cloneValueForStore(valueToStore)},
		})
	}
	if module != nil && module.vm != nil {
		_, checkedCapacity, err := module.vm.checkCollectionSize(int64(len(entries)), int64(capacity))
		if err != nil {
			return vmValue{}, err
		}
		capacity = checkedCapacity
		if err := module.vm.chargeRuntimeObject(0, capacity); err != nil {
			return vmValue{}, err
		}
	}
	out := newVMMap(capacity)
	for _, prepared := range entries {
		out.storeEntry(out.keyForStore(prepared.key), prepared.entry)
	}
	return newVMValue(runtimeType, out), nil
}

func newStructValue(module *moduleInstance, typ any, fields []string, values []vmValue) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	schema, ok := module.structSchema(runtimeType)
	if !ok {
		return vmValue{}, fmt.Errorf("unknown struct type %s", runtimeType)
	}
	return newStructValueWithSchema(module, runtimeType, schema, fields, values)
}

func newStructValueWithSchema(module *moduleInstance, runtimeType vmType, schema *structSchema, fields []string, values []vmValue) (vmValue, error) {
	if len(fields) != len(values) {
		return vmValue{}, fmt.Errorf("struct construction has %d fields and %d values", len(fields), len(values))
	}
	if schema == nil {
		return vmValue{}, fmt.Errorf("unknown struct type %s", runtimeType)
	}
	var slots []vmValue
	if len(fields) != 0 {
		slots = make([]vmValue, len(schema.fields))
	}
	for i, field := range fields {
		index, fieldInfo, ok := schema.field(field)
		if !ok {
			return vmValue{}, fmt.Errorf("unknown field %q for %s", field, runtimeType)
		}
		normalized, err := module.coerceAssignableRuntimeValue(values[i], fieldInfo.RuntimeType, fieldInfo.Variadic)
		if err != nil {
			return vmValue{}, fmt.Errorf("field %q: %w", field, err)
		}
		slots[index] = module.cloneValueForStore(normalized)
	}
	return newVMValue(runtimeType, &vmStruct{schema: schema, values: slots}), nil
}

func (m *moduleInstance) cloneValueForStore(value vmValue) vmValue {
	if !value.Type.Valid() {
		return value
	}
	if value.Type.Ref.Kind == types.Any || value.Type.ShapeKind() == types.Interface {
		if inner, ok := value.Data.(vmValue); ok {
			value.Data = m.cloneValueForStore(inner)
		}
		return value
	}
	switch value.Type.Ref.Kind {
	case types.Void, types.Primitive, types.Map, types.Pointer, types.Waitable, types.Function:
		return value
	}
	switch value.Type.ShapeKind() {
	case types.Slice:
		return value
	case types.Array:
		array, ok := value.Data.(*vmArray)
		if !ok {
			return value
		}
		items := array.values()
		out := make([]vmValue, len(items))
		for i, item := range items {
			out[i] = m.cloneValueForStore(item)
		}
		return newVMValue(value.Type, out)
	case types.Struct:
		data, ok := value.Data.(*vmStruct)
		if !ok || data == nil {
			return value
		}
		values, sparse := data.snapshot()
		for index, field := range values {
			values[index] = m.cloneValueForStore(field)
		}
		value.Data = &vmStruct{schema: data.schema, values: values, sparse: sparse}
		return value
	}
	return value
}

// assignPreparedValue installs an already copied value into addressable storage.
// Arrays keep their backing so existing element pointers and slices continue to
// refer to the variable, including arrays nested inside other aggregate values.
func (m *moduleInstance) assignPreparedValue(current, prepared vmValue) vmValue {
	if !current.Type.Equal(prepared.Type) {
		return prepared
	}
	switch destination := current.Data.(type) {
	case *vmArray:
		source, ok := prepared.Data.(*vmArray)
		if !ok || destination == nil || source == nil || destination.Len != source.Len {
			return prepared
		}
		for i, value := range source.values() {
			if !destination.ByteBacked {
				value = m.assignPreparedValue(destination.valueAt(i), value)
			}
			_ = destination.setValueAt(i, value)
		}
		return current
	case *vmStruct:
		source, ok := prepared.Data.(*vmStruct)
		if !ok || destination == nil || source == nil {
			return prepared
		}
		values, sparse := source.snapshot()
		previous, _ := destination.snapshot()
		if len(values) < len(previous) {
			values = append(values, make([]vmValue, len(previous)-len(values))...)
		}
		for i := range values {
			if i < len(previous) {
				if !values[i].Type.Valid() && previous[i].Type.Valid() {
					values[i] = m.zeroValue(source.schema.fields[i].RuntimeType)
				}
				values[i] = m.assignPreparedValue(previous[i], values[i])
			}
		}
		destination.mu.Lock()
		destination.values = values
		destination.sparse = sparse
		destination.mu.Unlock()
		return current
	}
	return prepared
}

func (m *moduleInstance) cloneValueForResult(value vmValue) vmValue {
	return m.cloneValueForStore(value)
}
