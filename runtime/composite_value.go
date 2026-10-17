package runtime

import (
	"errors"
	"fmt"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
)

type vmSlice struct {
	*vmSliceStorage
	// ByteBacking remains the caller-owned boundary view for detached byte
	// slices. Managed slices use the numeric page ticket below; this alias
	// preserves zero-copy FFI and view ownership at the host boundary.
	ByteBacking    []byte
	Start          int
	Len            int
	Cap            int
	managedHeap    *managedHeap
	managedRef     guestRef
	managedType    vmType
	managedElem    preparedValueLayout
	managedByte    bool
	managedLayouts *preparedLayoutRegistry
	managedBase    uint32
	managedOwner   *vm
	// inlineRaw is a typed view over a frame bank value. It deliberately does
	// not own a []vmValue mirror: element reads decode only the requested lane
	// and writes go straight back to the frame bytes.
	inlineRaw      []byte
	inlineType     vmType
	inlineElemType vmType
	inlineElem     preparedValueLayout
	inlineModule   *moduleInstance
	inlineBatch    *managedAllocationBatch
}

type vmSliceStorage struct {
	mu         sync.Mutex
	elements   []vmValue
	bytes      []byte
	capacity   int
	ByteBacked bool
}

func (s *vmSlice) elementIdentity(index int64) string {
	if s.managedHeap != nil && s.managedRef.valid() {
		offset := uint64(s.managedBase) + uint64(int64(s.Start)+index)*uint64(s.managedElem.Stride)
		return fmt.Sprintf("storage:%p:%d:%d:%d", s.managedHeap, s.managedRef.slot, s.managedRef.generation, offset)
	}
	if len(s.inlineRaw) != 0 {
		return fmt.Sprintf("inline:%p:%d", &s.inlineRaw[0], int64(s.Start)+index)
	}
	return fmt.Sprintf("slice:%p.index:%d", s.vmSliceStorage, int64(s.Start)+index)
}

func (s *vmSlice) byteBacked() bool {
	if s == nil {
		return false
	}
	if s.managedHeap != nil && s.managedRef.valid() {
		return s.managedByte
	}
	if s.inlineRaw != nil {
		return s.inlineElemType.Primitive(types.PrimitiveUint8)
	}
	return s.vmSliceStorage != nil && s.vmSliceStorage.ByteBacked
}

func (storage *vmSliceStorage) valueAt(index int) vmValue {
	if storage == nil || index < 0 || index >= len(storage.elements) {
		return vmValue{}
	}
	return storage.elements[index]
}

func (storage *vmSliceStorage) setValueAt(index int, value vmValue) {
	if storage != nil && index >= 0 && index < len(storage.elements) {
		storage.elements[index] = value
	}
}

func (storage *vmSliceStorage) byteAt(index int) byte {
	if storage == nil || index < 0 || index >= len(storage.bytes) {
		return 0
	}
	return storage.bytes[index]
}

func (storage *vmSliceStorage) setByteAt(index int, value byte) {
	if storage != nil && index >= 0 && index < len(storage.bytes) {
		storage.bytes[index] = value
	}
}

func newValueSliceStorage(backing []vmValue) *vmSliceStorage {
	return &vmSliceStorage{capacity: len(backing), elements: backing}
}

func newByteSliceStorage(backing []byte) *vmSliceStorage {
	return &vmSliceStorage{capacity: len(backing), bytes: backing, ByteBacked: true}
}

type vmArray struct {
	vmSlice
}

func (s *vmSlice) byteAt(index int) byte {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		if index < 0 || index >= s.Cap {
			return 0
		}
		var byteValue [1]byte
		if err := s.managedHeap.read(s.managedRef, s.managedBase+uint32(index), byteValue[:]); err != nil {
			return 0
		}
		return byteValue[0]
	}
	if s != nil && s.inlineRaw != nil {
		if index < 0 || index >= s.Cap || index >= len(s.inlineRaw) {
			return 0
		}
		return s.inlineRaw[index]
	}
	return s.vmSliceStorage.byteAt(index)
}

func (s *vmSlice) setByteAt(index int, value byte) {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		if index >= 0 && index < s.Cap {
			_ = s.managedHeap.write(s.managedRef, s.managedBase+uint32(index), []byte{value})
		}
		return
	}
	if s != nil && s.inlineRaw != nil {
		if index >= 0 && index < s.Cap && index < len(s.inlineRaw) {
			s.inlineRaw[index] = value
		}
		return
	}
	s.vmSliceStorage.setByteAt(index, value)
}

func (s *vmSlice) values() []vmValue {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		values := make([]vmValue, s.Len)
		for index := range values {
			values[index] = s.valueAt(index)
		}
		return values
	}
	if s != nil && s.inlineRaw != nil {
		values := make([]vmValue, s.Len)
		for index := range values {
			values[index] = s.valueAt(index)
		}
		return values
	}
	if s == nil || s.vmSliceStorage == nil {
		if s == nil || s.Len == 0 {
			return nil
		}
		return make([]vmValue, s.Len)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byteBacked() {
		values := make([]vmValue, s.Len)
		for index := range values {
			values[index] = newVMValue("Uint8", uint64(s.byteAt(s.Start+index)))
		}
		return values
	}
	values := make([]vmValue, s.Len)
	for index := range values {
		values[index] = s.vmSliceStorage.valueAt(s.Start + index)
	}
	return values
}

func (s *vmSlice) bytes() []byte {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		values := make([]byte, s.Len)
		if s.managedByte {
			if err := s.managedHeap.read(s.managedRef, s.managedBase+uint32(s.Start), values); err != nil {
				panic(err)
			}
			return values
		}
		for index := range values {
			values[index] = s.byteAt(s.Start + index)
		}
		return values
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]byte, s.Len)
	for index := range values {
		values[index] = s.byteAt(s.Start + index)
	}
	return values
}

func (s *vmSlice) writeBytes(offset int, values []byte) {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		if s.managedByte {
			if err := s.managedHeap.write(s.managedRef, s.managedBase+uint32(s.Start+offset), values); err != nil {
				panic(err)
			}
			return
		}
		for index, value := range values {
			s.setByteAt(s.Start+offset+index, value)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byteBacked() {
		for index, value := range values {
			s.setByteAt(s.Start+offset+index, value)
		}
		return
	}
	for index, value := range values {
		s.vmSliceStorage.setValueAt(s.Start+offset+index, newVMValue("Uint8", uint64(value)))
	}
}

// copyBytesFrom snapshots distinct storage before taking the destination lock.
// Views of the same storage can use copy directly, including overlapping ranges.
func (s *vmSlice) copyBytesFrom(source *vmSlice, count int) {
	if s.managedHeap != nil || source.managedHeap != nil {
		if s.managedHeap != nil && s.managedHeap == source.managedHeap && s.managedByte && source.managedByte {
			if err := s.managedHeap.copy(s.managedRef, s.managedBase+uint32(s.Start), source.managedRef, source.managedBase+uint32(source.Start), uint32(count)); err != nil {
				panic(err)
			}
			return
		}
		s.writeBytes(0, source.bytes()[:count])
		return
	}
	if s.vmSliceStorage == source.vmSliceStorage {
		s.mu.Lock()
		bytes := make([]byte, count)
		for index := 0; index < count; index++ {
			bytes[index] = source.byteAt(source.Start + index)
		}
		for index, value := range bytes {
			s.setByteAt(s.Start+index, value)
		}
		s.mu.Unlock()
		return
	}
	source.mu.Lock()
	bytes := make([]byte, count)
	for index := range bytes {
		bytes[index] = source.byteAt(source.Start + index)
	}
	source.mu.Unlock()
	s.writeBytes(0, bytes)
}

func (storage *vmSliceStorage) valuesSnapshot() []vmValue {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.ByteBacked {
		return nil
	}
	values := make([]vmValue, storage.capacity)
	for index := range values {
		values[index] = storage.valueAt(index)
	}
	return values
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
		vmSliceStorage: newByteSliceStorage(backing),
		ByteBacking:    backing,
		Len:            length,
		Cap:            capacity,
	})
}

func (s *vmSlice) valueAt(index int) vmValue {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		return readManagedSliceElement(s, index)
	}
	if s != nil && s.inlineRaw != nil {
		if index < 0 || index >= s.Len || s.inlineModule == nil || s.inlineElem.Stride == 0 {
			return vmValue{}
		}
		start := uint64(s.Start+index) * uint64(s.inlineElem.Stride)
		end := start + uint64(s.inlineElem.Stride)
		if end > uint64(len(s.inlineRaw)) {
			return vmValue{}
		}
		return readInlineAggregateOwned(s.inlineModule, s.inlineElemType, s.inlineElem, s.inlineRaw[start:end], s.inlineBatch)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byteBacked() {
		return newVMValue("Uint8", uint64(s.byteAt(s.Start+index)))
	}
	return s.vmSliceStorage.valueAt(s.Start + index)
}

func (s *vmSlice) setValueAt(index int, value vmValue) error {
	if s != nil && s.managedHeap != nil && s.managedRef.valid() {
		return writeManagedSliceElement(s, index, value)
	}
	if s != nil && s.inlineRaw != nil {
		if index < 0 || index >= s.Cap || s.inlineModule == nil || s.inlineElem.Stride == 0 {
			return errors.New("inline slice index out of range")
		}
		start := (s.Start + index) * int(s.inlineElem.Stride)
		if start < 0 || start+int(s.inlineElem.Stride) > len(s.inlineRaw) {
			return errors.New("inline slice storage is unavailable")
		}
		return s.inlineModule.writeInlineValueWithLayout(s.inlineRaw, start, value, s.inlineElem, map[uintptr]guestRef{}, s.inlineBatch)
	}
	if s.byteBacked() {
		n, err := numericAsUint64(value)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.setByteAt(s.Start+index, byte(n))
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.vmSliceStorage.setValueAt(s.Start+index, value)
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
		vmSliceStorage: newValueSliceStorage(backing),
		Start:          start,
		Len:            length,
		Cap:            capacity,
	})
}

func newSliceViewValue(typ any, source *vmSlice, start, length, capacity int) vmValue {
	if source == nil {
		return newSliceHeaderValue(typ, nil, start, length, capacity)
	}
	view := *source
	view.Start, view.Len, view.Cap = start, length, capacity
	return newVMValue(typ, &view)
}

func sliceValues(value vmValue) ([]vmValue, bool) {
	switch data := value.payload().(type) {
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

func newSequenceValueWithBatch(module *moduleInstance, typ any, values []vmValue, batch *managedAllocationBatch) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	typeText := runtimeType.String()
	isArray := false
	if length, _, ok := module.arrayType(typeText); ok {
		isArray = true
		if int64(len(values)) != length {
			return vmValue{}, fmt.Errorf("array length mismatch: got %d, want %d", len(values), length)
		}
	}
	elemType := module.sequenceElementType(typeText)
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
			result := newByteSliceHeaderValue(runtimeType, bytes, len(bytes), cap(bytes))
			if batch == nil {
				return result, nil
			}
			return module.importDetachedValueWithBatch(result, map[uintptr]guestRef{}, batch)
		}
		result := newSliceValue(runtimeType, out)
		if batch == nil {
			return result, nil
		}
		return module.importDetachedValueWithBatch(result, map[uintptr]guestRef{}, batch)
	}
	result := newVMValue(runtimeType, out)
	if batch == nil {
		return result, nil
	}
	return module.importDetachedValueWithBatch(result, map[uintptr]guestRef{}, batch)
}

func newMapValue(module *moduleInstance, typ any, pairs []vmValue) (vmValue, error) {
	return newMapValueWithCapacityBatch(module, typ, pairs, len(pairs)/2, nil)
}

func newMapValueWithCapacityBatch(module *moduleInstance, typ any, pairs []vmValue, capacity int, batch *managedAllocationBatch) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	typeText := runtimeType.String()
	if len(pairs)%2 != 0 {
		return vmValue{}, errors.New("map construction requires key/value pairs")
	}
	if capacity < len(pairs)/2 {
		capacity = len(pairs) / 2
	}
	entries := make([]managedMapSeed, 0, len(pairs)/2)
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
		entries = append(entries, managedMapSeed{
			key: key, entry: vmMapEntry{Key: module.cloneValueForStore(keyValue), Value: module.cloneValueForStore(valueToStore)},
		})
	}
	if module != nil && module.vm != nil {
		_, checkedCapacity, err := module.vm.checkCollectionSize(int64(len(entries)), int64(capacity))
		if err != nil {
			return vmValue{}, err
		}
		capacity = checkedCapacity
		result, err := module.newManagedMapWithBatch(runtimeType, entries, capacity, batch)
		if err != nil {
			return vmValue{}, err
		}
		return result, nil
	}
	return module.newManagedMapWithBatch(runtimeType, entries, capacity, batch)
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
	return newStructValueWithSchemaBatch(module, runtimeType, schema, fields, values, nil)
}

func newStructValueWithSchemaBatch(module *moduleInstance, runtimeType vmType, schema *structSchema, fields []string, values []vmValue, batch *managedAllocationBatch) (vmValue, error) {
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
	result := newVMValue(runtimeType, &vmStruct{schema: schema, values: slots})
	if batch == nil {
		return result, nil
	}
	return module.importDetachedValueWithBatch(result, map[uintptr]guestRef{}, batch)
}

func (m *moduleInstance) cloneValueForStore(value vmValue) vmValue {
	if !value.Type.Valid() {
		return value
	}
	// An owned value may outlive the activation that supplied its payload.
	// Keep the decoded scalar/reference value, not a shortcut back into a
	// reusable frame bank. Aggregates acquire their own storage below.
	value.inlineOwner, value.inlineIndex, value.inlineTemp = nil, 0, false
	if value.Type.Ref.Kind == types.Any || value.Type.ShapeKind() == types.Interface {
		if inner, ok := value.payload().(vmValue); ok {
			value = m.detachedValue(value, m.cloneValueForStore(inner))
		}
		return value
	}
	if m != nil && m.vm != nil && (value.Type.ShapeKind() == types.Array || value.Type.ShapeKind() == types.Struct) {
		cell, err := m.newManagedCell(value.Type, value)
		if err != nil {
			m.vm.recordAllocationFailure(fmt.Errorf("copy aggregate %s: %w", value.Type, err))
			return vmValue{}
		}
		return vmValue{Type: value.Type, ref: cell.ref, heap: cell.heap, owner: m.vm, module: m, layouts: m.vm.valueLayouts}
	}
	switch value.Type.Ref.Kind {
	case types.Void, types.Primitive, types.Map, types.Pointer, types.Waitable, types.Function:
		return value
	}
	switch value.Type.ShapeKind() {
	case types.Slice:
		return value
	case types.Array:
		array, ok := value.payload().(*vmArray)
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
		data, ok := value.payload().(*vmStruct)
		if !ok || data == nil {
			return value
		}
		if data.managedHeap != nil && data.managedRef.valid() {
			values := make([]vmValue, len(data.managedFields))
			for index := range values {
				if field, found := data.fieldAt(index); found {
					values[index] = m.cloneValueForStore(field)
				}
			}
			return m.detachedValue(value, &vmStruct{schema: data.schema, values: values})
		}
		if data.inlineRaw != nil {
			values, sparse := data.snapshot()
			for index, field := range values {
				if field.Type.Valid() {
					values[index] = m.cloneValueForStore(field)
				}
			}
			return m.detachedValue(value, &vmStruct{schema: data.schema, values: values, sparse: sparse})
		}
		data.mu.Lock()
		// Reference-like fields already have Go copy semantics. Nested value
		// aggregates still need independent addressable storage.
		shareable := true
		for _, field := range data.values {
			for field.Type.Ref.Kind == types.Any || field.Type.ShapeKind() == types.Interface {
				if field.payload() == nil {
					field = vmValue{}
					break
				}
				inner, ok := field.payload().(vmValue)
				if !ok {
					shareable = false
					break
				}
				field = inner
			}
			if !shareable {
				break
			}
			switch field.Type.Ref.Kind {
			case types.Void, types.Primitive, types.Map, types.Pointer, types.Waitable, types.Function:
				continue
			}
			if field.Type.ShapeKind() != types.Slice && field.Type.Valid() {
				shareable = false
				break
			}
		}
		if shareable {
			data.shared = true
			value = m.detachedValue(value, &vmStruct{schema: data.schema, values: data.values, sparse: data.sparse, shared: true})
			data.mu.Unlock()
			return value
		}
		data.mu.Unlock()
		values, sparse := data.snapshot()
		for index, field := range values {
			values[index] = m.cloneValueForStore(field)
		}
		value = m.detachedValue(value, &vmStruct{schema: data.schema, values: values, sparse: sparse})
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
	if current.inlineOwner != nil {
		// The owning frame publishes the replacement into the same byte bank.
		// Updating a materialized field slice here would leave the canonical
		// inline bytes unchanged on the second assignment to a loop variable.
		return prepared
	}
	if current.ref.valid() && current.heap != nil && (current.Type.ShapeKind() == types.Array || current.Type.ShapeKind() == types.Struct || current.Type.ShapeKind() == types.Slice) {
		return prepared
	}
	if structure, ok := current.Data.(*vmStruct); ok && structure != nil && (structure.inlineRaw != nil || structure.managedHeap != nil && structure.managedRef.valid()) {
		return prepared
	}
	switch destination := current.payload().(type) {
	case *vmArray:
		source, ok := prepared.payload().(*vmArray)
		if !ok || destination == nil || source == nil || destination.Len != source.Len {
			return prepared
		}
		for i, value := range source.values() {
			if !destination.byteBacked() {
				value = m.assignPreparedValue(destination.valueAt(i), value)
			}
			_ = destination.setValueAt(i, value)
		}
		return current
	case *vmStruct:
		source, ok := prepared.payload().(*vmStruct)
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
		destination.shared = false
		destination.mu.Unlock()
		return current
	}
	return prepared
}

func (m *moduleInstance) cloneValueForResult(value vmValue) vmValue {
	payload := value.payload()
	switch data := payload.(type) {
	case vmValue:
		return newVMValue(value.Type, m.cloneValueForResult(data))
	case string:
		return newVMValue(value.Type, data)
	case *vmSlice:
		if data == nil {
			return newVMValue(value.Type, (*vmSlice)(nil))
		}
		values := data.values()
		if data.byteBacked() {
			return newByteSliceHeaderValue(value.Type, data.bytes(), data.Len, data.Cap)
		}
		for index := range values {
			values[index] = m.cloneValueForResult(values[index])
		}
		return newSliceValue(value.Type, values)
	case *vmArray:
		if data == nil {
			return newVMValue(value.Type, []vmValue(nil))
		}
		values := data.values()
		for index := range values {
			values[index] = m.cloneValueForResult(values[index])
		}
		return newVMValue(value.Type, values)
	case *vmStruct:
		if data == nil {
			return value
		}
		var values []vmValue
		if data.managedHeap != nil && data.managedRef.valid() {
			values = make([]vmValue, len(data.managedFields))
			for index := range values {
				if field, ok := data.fieldAt(index); ok {
					values[index] = field
				}
			}
		} else {
			values, _ = data.snapshot()
		}
		for index := range values {
			values[index] = m.cloneValueForResult(values[index])
		}
		return newVMValue(value.Type, &vmStruct{schema: data.schema, values: values})
	default:
		if payload != nil {
			value.ref, value.heap, value.owner, value.layouts = guestRef{}, nil, nil, nil
			value.Data = payload
		}
		return value
	}
}
