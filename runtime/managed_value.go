package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

// managedValueRepresentation is the small, fixed representation field used
// by dynamic slots.  It is deliberately independent from the Go payload type:
// the page stores bits and a guestRef, while a detached vmValue is materialized
// only at a host or reflection boundary.
type managedValueRepresentation uint32

const (
	managedValueEmpty managedValueRepresentation = iota
	managedValueBits
	managedValueTypedNil
	managedValueTypedZero
	managedValueRef
	managedValueBox
)

// managedValueEncoding is the register-sized view used by opcode consumers.
// It owns no guest object and therefore can safely live in a frame temporary.
// lo/hi contain scalar or descriptor bits; ref is the complete slot and
// generation ticket whenever representation is Ref or Box.
type managedValueEncoding struct {
	typ            vmType
	representation managedValueRepresentation
	lo             uint64
	hi             uint64
	ref            guestRef
}

type managedStructField struct {
	offset uint32
	typ    vmType
	layout preparedValueLayout
}

// adoptValue only publishes values that already have a numeric representation
// (scalar lanes or a page ticket). Aggregate import is performed by the
// prepared layout codec at the construction boundary; it must not recurse
// through Data or create an owning host graph here.
func (module *moduleInstance) adoptValue(value vmValue) vmValue {
	return module.adoptValueWithBatch(value, nil)
}

func (module *moduleInstance) adoptValueWithBatch(value vmValue, batch *managedAllocationBatch) vmValue {
	if module == nil || module.vm == nil || module.vm.heap == nil {
		return value
	}
	value.Type = module.resolvedRuntimeType(value.Type)
	value.inlineOwner, value.inlineIndex, value.inlineTemp = nil, 0, false
	aggregateZero := value.Data == nil && (value.Type.ShapeKind() == types.Array || value.Type.ShapeKind() == types.Struct)
	if value.scalarKind != 0 || value.Type.Primitive(types.PrimitiveBool) || value.ref.valid() || value.Data == nil && !aggregateZero {
		if value.ref.valid() {
			value.heap, value.owner, value.module, value.layouts = module.vm.heap, module.vm, module, module.vm.valueLayouts
			module.vm.trackReflectTypeValue(value)
		}
		return value
	}
	imported, err := module.importDetachedValueWithBatch(value, map[uintptr]guestRef{}, batch)
	if err != nil {
		if batch != nil {
			batch.failure = fmt.Errorf("publish managed value %s: %w", value.Type, err)
			return vmValue{}
		}
		// Every published aggregate must have a numeric page representation.
		// Keeping the detached payload would create an untraced guest graph and
		// make a later collection silently lose it.
		module.vm.recordAllocationFailure(fmt.Errorf("publish managed value %s: %w", value.Type, err))
		return vmValue{}
	}
	module.vm.trackReflectTypeValue(imported)
	return imported
}

//nolint:staticcheck // moduleInstance uses the established module receiver in this file.
func (module *moduleInstance) adoptChild(child vmValue) vmValue {
	return module.adoptValue(child)
}

func (module *moduleInstance) importDetachedValue(value vmValue, seen map[uintptr]guestRef) (vmValue, error) {
	return module.importDetachedValueWithBatch(value, seen, nil)
}

func (module *moduleInstance) importDetachedValueWithBatch(value vmValue, seen map[uintptr]guestRef, batch *managedAllocationBatch) (vmValue, error) {
	if module == nil || module.vm == nil || module.vm.heap == nil {
		return value, errors.New("managed value has no heap owner")
	}
	value.Type = module.resolvedRuntimeType(value.Type)
	value.inlineOwner, value.inlineIndex, value.inlineTemp = nil, 0, false
	if value.ref.valid() && value.heap == module.vm.heap {
		value.owner, value.module, value.layouts = module.vm, module, module.vm.valueLayouts
		module.vm.trackReflectTypeValue(value)
		return value, nil
	}
	if !value.ref.valid() && isNilRuntimeValue(value) {
		switch value.Type.ShapeKind() {
		case types.Pointer, types.Map, types.Waitable, types.Function:
			// A nil reference is the zero ticket, including when nested in an
			// aggregate. Allocating a zero-filled object would invent identity
			// and make subsequent resource initialization target the wrong value.
			value.Data = nil
			return value, nil
		}
	}
	// A view produced by pointer-to-array conversion already names the
	// backing record and byte offset. Re-publishing it through a stable cell
	// must retain that location; importing its materialized elements would
	// allocate a detached array and silently break aliasing with the source
	// slice.
	if array, ok := value.Data.(*vmArray); ok && array != nil && array.managedHeap == module.vm.heap && array.managedRef.valid() {
		stride := array.managedElem.Stride
		if stride == 0 {
			layout, layoutErr := module.vm.valueLayouts.layoutFor(array.managedType)
			if layoutErr != nil {
				return vmValue{}, layoutErr
			}
			stride = layout.Stride
		}
		value.ref = array.managedRef
		value.heap = module.vm.heap
		value.offset = array.managedBase + uint32(array.Start)*stride
		value.owner, value.module, value.layouts = module.vm, module, module.vm.valueLayouts
		value.Data = nil
		return value, nil
	}
	if value.Type.ShapeKind() == types.Map {
		if data, ok := value.Data.(*vmMap); ok && data != nil {
			return module.importManagedMap(value, data)
		}
	}
	if value.Type.ShapeKind() == types.Waitable {
		if resource, ok := value.Data.(*mutexResource); ok && resource != nil {
			return module.importManagedMutex(value, resource, batch)
		}
		if data, ok := value.Data.(*waitableResource); ok && data != nil {
			return module.importManagedWaitable(value, data)
		}
	}
	if value.Type.ShapeKind() == types.Pointer {
		if pointer, ok := value.Data.(*vmPointer); ok && pointer != nil {
			return module.newManagedPointer(pointer)
		}
	}
	if value.Type.ShapeKind() == types.Function || value.Type.Primitive(types.PrimitiveFunction) {
		switch value.Data.(type) {
		case functionRef, reflectMethodTarget, reflectUnboundMethodTarget, reflectMakeFuncTarget:
			return module.newManagedFunction(value.Type, value.Data, batch)
		}
	}
	if payload := value.Data; payload != nil {
		if identity, ok := managedDetachedIdentity(payload); ok {
			if ref, exists := seen[identity]; exists {
				value.ref, value.heap, value.owner, value.layouts, value.Data = ref, module.vm.heap, module.vm, module.vm.valueLayouts, nil
				return value, nil
			}
		}
	}
	layout, err := module.vm.valueLayouts.layoutFor(value.Type)
	if err != nil {
		return vmValue{}, err
	}
	heapLayout, err := module.vm.heap.registerLayout(managedLayout{stride: layout.Stride, typeID: layout.ID, references: layout.References, dynamics: layout.Dynamics, types: layout.TypeOffsets})
	if err != nil {
		return vmValue{}, err
	}
	defer module.vm.heap.releaseLayout(heapLayout)
	logical, valid := bytecode.CheckedStorageBytes(bytecode.StorageRecordHeaderBytes, 1, layout.Logical)
	if !valid {
		return vmValue{}, errManagedLimit
	}
	reservation, err := module.vm.heap.reserveWithBatch(batch, heapLayout, 1, logical)
	if err != nil {
		return vmValue{}, err
	}
	if payload := value.Data; payload != nil {
		if identity, ok := managedDetachedIdentity(payload); ok {
			seen[identity] = reservation.reference()
		}
	}
	bytes := reservation.bytes()
	if err := module.initializeManagedValueWithBatch(bytes, layout, value, seen, batch); err != nil {
		reservation.abort()
		return vmValue{}, err
	}
	ref := reservation.commit()
	if !ref.valid() {
		return vmValue{}, errors.New("managed value publication failed")
	}
	value.ref, value.heap, value.owner, value.module, value.layouts, value.Data = ref, module.vm.heap, module.vm, module, module.vm.valueLayouts, nil
	module.vm.trackReflectTypeValue(value)
	return value, nil
}

func (module *moduleInstance) initializeManagedValue(dst []byte, layout preparedValueLayout, value vmValue, seen map[uintptr]guestRef) error {
	return module.initializeManagedValueWithBatch(dst, layout, value, seen, nil)
}

func (module *moduleInstance) initializeManagedValueWithBatch(dst []byte, layout preparedValueLayout, value vmValue, seen map[uintptr]guestRef, batch *managedAllocationBatch) error {
	if len(dst) < int(layout.Stride) {
		return errors.New("managed value page is smaller than prepared layout")
	}
	shape := layout.Shape
	primitive := layout.Primitive
	numeric := layout.NumericKind != types.NumericInvalid
	if shape == types.Invalid {
		shape = value.Type.ShapeKind()
		if underlying := value.Type.Underlying(); underlying.Ref.Kind == types.Primitive {
			primitive = underlying.Ref.Primitive
		}
		_, numeric = value.Type.NumericInfo()
	}
	if shape == types.Array || shape == types.Struct {
		if copied, err := copyAggregateStorage(dst[:layout.Stride], value); copied || err != nil {
			return err
		}
	}
	if numeric {
		return writeScalarBytes(dst, 0, value)
	}
	if primitive == types.PrimitiveBool {
		if boolean, ok := value.boolValue(); ok && boolean {
			dst[0] = 1
		}
		return nil
	}
	if shape == types.Any || shape == types.Interface {
		encoded, err := module.dynamicEncodingWithBatch(value, seen, batch)
		if err != nil {
			return err
		}
		module.vm.trackReflectTypeEncoding(value, encoded)
		dynamicLayout, err := module.vm.valueLayouts.layoutFor(encoded.typ)
		if err != nil {
			return err
		}
		return encodeManagedDynamic(dst, dynamicLayout.ID, encoded)
	}
	switch data := value.Data.(type) {
	case vmValue:
		// Trusted runtime constructors wrap a concrete struct payload in a
		// vmValue while changing the declared result type (for example
		// reflect.Type). Encode the inner payload with the outer prepared
		// layout; do not route it through dynamicEncoding, which would recurse
		// on the wrapper itself.
		return module.initializeManagedValueWithBatch(dst, layout, data, seen, batch)
	case *vmArray:
		return module.initializeSliceElementsWithBatch(dst, layout, data.values(), seen, batch)
	case *vmSlice:
		return module.initializeSliceValueWithBatch(dst, value.Type, data, seen, batch)
	case *vmStruct:
		values, _ := data.snapshot()
		return module.initializeStructFieldsWithBatch(dst, layout, data, values, seen, batch)
	case string:
		return module.initializeStringValueWithBatch(dst, data, batch)
	default:
		// Runtime resources use the numeric record as their identity. The
		// transient host object is kept in the VM handle table, never in the
		// managed page or a recursive guest object graph.
		clear(dst[:layout.Stride])
		return nil
	}
}

// copyAggregateStorage copies an aggregate's prepared lanes without decoding
// its children. Value fields are inline; reference fields retain their tickets.
// The destination is private scratch or an unpublished reservation, so its owner
// publishes the complete write (and the required cards) after this returns.
func copyAggregateStorage(dst []byte, value vmValue) (bool, error) {
	if value.ref.valid() && value.heap != nil {
		return true, value.heap.read(value.ref, value.offset, dst)
	}
	var source []byte
	switch data := value.Data.(type) {
	case *vmArray:
		if data == nil {
			return false, nil
		}
		if data.managedHeap != nil && data.managedRef.valid() {
			return true, data.managedHeap.read(data.managedRef, data.managedBase+uint32(data.Start)*data.managedElem.Stride, dst)
		}
		start := int64(data.Start) * int64(data.inlineElem.Stride)
		if data.inlineRaw != nil && start >= 0 && start <= int64(len(data.inlineRaw)) {
			source = data.inlineRaw[start:]
		}
	case *vmStruct:
		if data == nil {
			return false, nil
		}
		if data.managedHeap != nil && data.managedRef.valid() {
			return true, data.managedHeap.read(data.managedRef, data.managedBase, dst)
		}
		source = data.inlineRaw
	}
	if source == nil {
		return false, nil
	}
	if len(source) < len(dst) {
		return false, errors.New("aggregate view is smaller than its prepared layout")
	}
	copy(dst, source[:len(dst)])
	return true, nil
}

func (module *moduleInstance) initializeSliceElementsWithBatch(dst []byte, layout preparedValueLayout, values []vmValue, seen map[uintptr]guestRef, batch *managedAllocationBatch) error {
	if len(values) == 0 {
		return nil
	}
	stride := int(layout.Stride) / len(values)
	if stride <= 0 {
		return errors.New("invalid aggregate element stride")
	}
	for index, value := range values {
		if err := module.writeInlineValueWithBatch(dst, index*stride, value, seen, batch); err != nil {
			return err
		}
	}
	return nil
}

func (module *moduleInstance) initializeStructFieldsWithBatch(dst []byte, layout preparedValueLayout, structure *vmStruct, values []vmValue, seen map[uintptr]guestRef, batch *managedAllocationBatch) error {
	plan := layout.StructPlan
	if plan == nil {
		typ, ok := module.vm.valueLayouts.typeByID(layout.ID)
		if !ok {
			return errors.New("struct storage type is unavailable")
		}
		var err error
		plan, err = module.vm.valueLayouts.structLayout(typ)
		if err != nil {
			return err
		}
	}
	if len(values) == 0 {
		if structure == nil || structure.managedHeap == nil || !structure.managedRef.valid() {
			return nil
		}
		values = make([]vmValue, len(structure.managedFields))
		for index := range values {
			if field, found := structure.fieldAt(index); found {
				values[index] = field
			}
		}
	}
	for index, value := range values {
		if structure.schema == nil || index >= len(structure.schema.fields) {
			return errors.New("struct field schema is unavailable")
		}
		fieldIndex, _, found := plan.schema.field(structure.schema.fields[index].Name)
		if !found {
			return fmt.Errorf("struct field %q is unavailable", structure.schema.fields[index].Name)
		}
		field := plan.fields[fieldIndex]
		fieldType := field.typ
		if !value.Type.Valid() && fieldType.Valid() {
			value = module.zeroValue(fieldType)
		} else if value.Type.Equal(fieldType) {
			value.Type = fieldType
		}
		if err := module.writeInlineValueWithLayout(dst, int(field.offset), value, field.layout, seen, batch); err != nil {
			return fmt.Errorf("struct field %d: %w", index, err)
		}
	}
	return nil
}

func (module *moduleInstance) writeInlineValue(dst []byte, value vmValue, seen map[uintptr]guestRef) error {
	return module.writeInlineValueWithBatch(dst, 0, value, seen, nil)
}

func (module *moduleInstance) writeInlineValueWithBatch(dst []byte, offset int, value vmValue, seen map[uintptr]guestRef, batch *managedAllocationBatch) error {
	value.Type = module.resolvedRuntimeType(value.Type)
	layout, err := module.vm.valueLayouts.layoutFor(value.Type)
	if err != nil {
		return err
	}
	return module.writeInlineValueWithLayout(dst, offset, value, layout, seen, batch)
}

func (module *moduleInstance) writeInlineValueWithLayout(dst []byte, offset int, value vmValue, layout preparedValueLayout, seen map[uintptr]guestRef, batch *managedAllocationBatch) error {
	if offset < 0 || offset >= len(dst) {
		return errors.New("inline value starts outside prepared page")
	}
	if !layout.Prepared {
		value.Type = module.resolvedRuntimeType(value.Type)
	}
	if !layout.Prepared && layout.ID == 0 {
		resolvedLayout, err := module.vm.valueLayouts.layoutFor(value.Type)
		if err != nil {
			return err
		}
		layout = resolvedLayout
	}
	shape := layout.Shape
	primitive := layout.Primitive
	if shape == types.Invalid {
		shape = value.Type.ShapeKind()
		if underlying := value.Type.Underlying(); underlying.Ref.Kind == types.Primitive {
			primitive = underlying.Ref.Primitive
		}
		if !layout.Prepared {
			resolvedLayout, err := module.vm.valueLayouts.layoutFor(value.Type)
			if err != nil {
				return err
			}
			layout = resolvedLayout
		}
	}
	// Primitive lanes already carry their executable type. Their write does
	// not need the shared layout registry or any guest allocation.
	if layout.NumericKind != types.NumericInvalid {
		return writeScalarBytes(dst, offset, value)
	}
	if primitive == types.PrimitiveBool {
		return writeScalarBytes(dst, offset, value)
	}
	childLayout := layout
	if primitive == types.PrimitiveString {
		if offset+int(childLayout.Stride) > len(dst) {
			return errors.New("string exceeds prepared page")
		}
		if value.ref.valid() && value.Data == nil {
			return module.vm.heap.read(value.ref, value.offset, dst[offset:offset+int(childLayout.Stride)])
		}
		return module.initializeManagedValueWithBatch(dst[offset:offset+int(childLayout.Stride)], childLayout, value, seen, batch)
	}
	switch shape {
	case types.Array, types.Struct, types.Slice:
		if offset+int(childLayout.Stride) > len(dst) {
			return errors.New("aggregate exceeds prepared page")
		}
		if value.ref.valid() && value.Data == nil {
			return module.vm.heap.read(value.ref, value.offset, dst[offset:offset+int(childLayout.Stride)])
		}
		return module.initializeManagedValueWithBatch(dst[offset:offset+int(childLayout.Stride)], childLayout, value, seen, batch)
	case types.Pointer:
		if offset+int(childLayout.Stride) > len(dst) {
			return errors.New("pointer exceeds prepared page")
		}
		child, err := module.importDetachedValueWithBatch(value, seen, batch)
		if err != nil {
			return err
		}
		if !child.ref.valid() {
			clear(dst[offset : offset+int(childLayout.Stride)])
			return nil
		}
		targetType, _ := child.Type.PointerElem()
		targetLayout, err := module.vm.valueLayouts.layoutFor(module.resolvedRuntimeType(targetType))
		if err != nil {
			return err
		}
		clear(dst[offset : offset+int(childLayout.Stride)])
		binary.LittleEndian.PutUint32(dst[offset:], child.ref.slot)
		binary.LittleEndian.PutUint32(dst[offset+4:], child.ref.generation)
		binary.LittleEndian.PutUint64(dst[offset+8:], uint64(child.offset))
		binary.LittleEndian.PutUint32(dst[offset+16:], targetLayout.ID)
		return nil
	case types.Any, types.Interface:
		encoded, err := module.dynamicEncodingWithBatch(value, seen, batch)
		if err != nil {
			return err
		}
		module.vm.trackReflectTypeEncoding(value, encoded)
		dynamicLayout, err := module.vm.valueLayouts.layoutFor(encoded.typ)
		if err != nil {
			return err
		}
		return encodeManagedDynamic(dst[offset:], dynamicLayout.ID, encoded)
	}
	child, err := module.importDetachedValueWithBatch(value, seen, batch)
	if err != nil {
		return err
	}
	if offset+8 > len(dst) {
		return errors.New("reference exceeds prepared page")
	}
	binary.LittleEndian.PutUint32(dst[offset:], child.ref.slot)
	binary.LittleEndian.PutUint32(dst[offset+4:], child.ref.generation)
	return nil
}

func writeScalarBytes(dst []byte, offset int, value vmValue) error {
	if value.ref.valid() {
		decoded, err := readManagedScalar(value.heap, value.ref, value.offset, value.Type)
		if err != nil {
			return err
		}
		value = decoded
	}
	info, _ := value.Type.NumericInfo()
	if info.Kind == types.NumericComplex {
		complexValue, ok := value.Data.(complex128)
		if !ok {
			return errors.New("complex scalar has no numeric payload")
		}
		componentWidth := info.Bits / 16
		if componentWidth == 0 {
			componentWidth = 4
		}
		if offset < 0 || offset+componentWidth*2 > len(dst) {
			return errors.New("complex scalar exceeds prepared page")
		}
		if componentWidth == 4 {
			binary.LittleEndian.PutUint32(dst[offset:], math.Float32bits(float32(real(complexValue))))
			binary.LittleEndian.PutUint32(dst[offset+componentWidth:], math.Float32bits(float32(imag(complexValue))))
		} else {
			binary.LittleEndian.PutUint64(dst[offset:], math.Float64bits(real(complexValue)))
			binary.LittleEndian.PutUint64(dst[offset+componentWidth:], math.Float64bits(imag(complexValue)))
		}
		return nil
	}
	width := 8
	if info.Bits > 0 {
		width = info.Bits / 8
	}
	if value.Type.Primitive(types.PrimitiveBool) {
		width = 1
	}
	if width < 1 {
		width = 1
	}
	if offset < 0 || offset+width > len(dst) {
		return errors.New("scalar exceeds prepared page")
	}
	if value.Type.Primitive(types.PrimitiveBool) {
		dst[offset] = 0
		if boolean, ok := value.boolValue(); ok && boolean {
			dst[offset] = 1
		}
		return nil
	}
	var encoded [8]byte
	bits := value.scalar
	if info.Kind == types.NumericFloat && info.Bits == 32 {
		bits = uint64(math.Float32bits(float32(math.Float64frombits(bits))))
	}
	binary.LittleEndian.PutUint64(encoded[:], bits)
	copy(dst[offset:offset+width], encoded[:width])
	return nil
}

func (module *moduleInstance) dynamicEncoding(value vmValue, seen map[uintptr]guestRef) (managedValueEncoding, error) {
	return module.dynamicEncodingWithBatch(value, seen, nil)
}

func (module *moduleInstance) dynamicEncodingWithBatch(value vmValue, seen map[uintptr]guestRef, batch *managedAllocationBatch) (managedValueEncoding, error) {
	value.Type = module.resolvedRuntimeType(value.Type)
	if value.scalarKind != 0 || value.Type.Primitive(types.PrimitiveBool) {
		return encodeManagedValue(value)
	}
	if value.ref.valid() {
		if value.Type.ShapeKind() == types.Any || value.Type.ShapeKind() == types.Interface {
			inner, ok := materializeManagedDynamic(value)
			if !ok {
				return managedValueEncoding{typ: value.Type, representation: managedValueEmpty}, nil
			}
			return module.dynamicEncodingWithBatch(inner, seen, batch)
		}
		return managedValueEncoding{typ: value.Type, representation: managedValueRef, ref: value.ref, hi: uint64(value.offset)}, nil
	}
	if isNilRuntimeValue(value) {
		if value.Type.ShapeKind() == types.Any || value.Type.ShapeKind() == types.Interface {
			return managedValueEncoding{typ: value.Type, representation: managedValueEmpty}, nil
		}
		return managedValueEncoding{typ: value.Type, representation: managedValueTypedNil}, nil
	}
	if inner, ok := value.Data.(vmValue); ok {
		return module.dynamicEncodingWithBatch(inner, seen, batch)
	}
	if _, ok := value.Data.(complex128); ok {
		return encodeManagedValue(value)
	}
	child, err := module.importDetachedValueWithBatch(value, seen, batch)
	if err != nil {
		return managedValueEncoding{}, err
	}
	return encodeManagedValue(child)
}

func (module *moduleInstance) initializeStringValueWithBatch(dst []byte, text string, batch *managedAllocationBatch) error {
	if len(dst) < 24 {
		return errors.New("string descriptor is smaller than 24 bytes")
	}
	backingLayout, err := module.vm.heap.registerLayout(managedLayout{stride: 1})
	if err != nil {
		return err
	}
	defer module.vm.heap.releaseLayout(backingLayout)
	reservation, err := module.vm.heap.reserveWithBatch(batch, backingLayout, uint32(len(text)), bytecode.StorageRecordHeaderBytes+uint64(len(text)))
	if err != nil {
		return err
	}
	copy(reservation.bytes(), text)
	backing := reservation.commit()
	binary.LittleEndian.PutUint32(dst, backing.slot)
	binary.LittleEndian.PutUint32(dst[4:], backing.generation)
	binary.LittleEndian.PutUint64(dst[16:], uint64(len(text)))
	return nil
}

func (module *moduleInstance) initializeSliceValueWithBatch(dst []byte, typ vmType, slice *vmSlice, seen map[uintptr]guestRef, batch *managedAllocationBatch) error {
	if len(dst) < 32 {
		return errors.New("invalid slice descriptor")
	}
	if slice == nil {
		clear(dst[:32])
		return nil
	}
	if slice.managedHeap == module.vm.heap && slice.managedRef.valid() {
		binary.LittleEndian.PutUint32(dst, slice.managedRef.slot)
		binary.LittleEndian.PutUint32(dst[4:], slice.managedRef.generation)
		offset := uint64(slice.managedBase) + uint64(slice.Start)*uint64(slice.managedElem.Stride)
		binary.LittleEndian.PutUint64(dst[8:], offset)
		binary.LittleEndian.PutUint64(dst[16:], uint64(slice.Len))
		binary.LittleEndian.PutUint64(dst[24:], uint64(slice.Cap))
		return nil
	}
	backingView := *slice
	backingView.Len = backingView.Cap
	if slice.byteBacked() {
		backingLayout, err := module.vm.heap.registerLayout(managedLayout{stride: 1})
		if err != nil {
			return err
		}
		defer module.vm.heap.releaseLayout(backingLayout)
		reservation, err := module.vm.heap.reserveWithBatch(batch, backingLayout, uint32(slice.Cap), bytecode.StorageRecordHeaderBytes+uint64(slice.Cap))
		if err != nil {
			return err
		}
		copy(reservation.bytes(), backingView.bytes())
		backing := reservation.commit()
		binary.LittleEndian.PutUint32(dst, backing.slot)
		binary.LittleEndian.PutUint32(dst[4:], backing.generation)
	} else {
		values := backingView.values()
		if len(values) == 0 {
			layout, err := module.vm.heap.registerLayout(managedLayout{stride: 1})
			if err != nil {
				return err
			}
			defer module.vm.heap.releaseLayout(layout)
			reservation, err := module.vm.heap.reserveWithBatch(batch, layout, 0, bytecode.StorageRecordHeaderBytes)
			if err != nil {
				return err
			}
			backing := reservation.commit()
			binary.LittleEndian.PutUint32(dst, backing.slot)
			binary.LittleEndian.PutUint32(dst[4:], backing.generation)
			return nil
		}
		element, ok := typ.SliceElem()
		if !ok {
			return errors.New("slice has no declared element type")
		}
		elementLayout, err := module.vm.valueLayouts.layoutFor(module.resolvedRuntimeType(element))
		if err != nil {
			return err
		}
		backingLayout, err := module.vm.heap.registerLayout(managedLayout{stride: elementLayout.Stride, typeID: elementLayout.ID, references: elementLayout.References, dynamics: elementLayout.Dynamics, types: elementLayout.TypeOffsets})
		if err != nil {
			return err
		}
		defer module.vm.heap.releaseLayout(backingLayout)
		logical, valid := bytecode.CheckedStorageBytes(bytecode.StorageRecordHeaderBytes, uint64(slice.Cap), elementLayout.Logical)
		if !valid {
			return errManagedLimit
		}
		reservation, err := module.vm.heap.reserveWithBatch(batch, backingLayout, uint32(slice.Cap), logical)
		if err != nil {
			return err
		}
		backingBytes := reservation.bytes()
		for index, item := range values {
			// Capacity beyond the initialized host slice is a typed zero range.
			// The reservation is already zeroed; it needs no temporary Value.
			if !item.Type.Valid() {
				continue
			}
			if err := module.writeInlineValueWithLayout(backingBytes, index*int(elementLayout.Stride), item, elementLayout, seen, batch); err != nil {
				reservation.abort()
				return err
			}
		}
		backing := reservation.commit()
		binary.LittleEndian.PutUint32(dst, backing.slot)
		binary.LittleEndian.PutUint32(dst[4:], backing.generation)
	}
	binary.LittleEndian.PutUint64(dst[8:], 0)
	binary.LittleEndian.PutUint64(dst[16:], uint64(slice.Len))
	binary.LittleEndian.PutUint64(dst[24:], uint64(slice.Cap))
	return nil
}

func materializeManagedString(value vmValue) (string, bool) {
	if value.heap == nil || !value.ref.valid() || !value.Type.Primitive(types.PrimitiveString) {
		return "", false
	}
	return materializeManagedStringAt(value.heap, value.ref, value.offset)
}

func materializeManagedStringAt(heap *managedHeap, ref guestRef, offset uint32) (string, bool) {
	if heap == nil || !ref.valid() {
		return "", false
	}
	var descriptor [24]byte
	if err := heap.read(ref, offset, descriptor[:]); err != nil {
		return "", false
	}
	backing := guestRef{slot: binary.LittleEndian.Uint32(descriptor[0:4]), generation: binary.LittleEndian.Uint32(descriptor[4:8])}
	start := binary.LittleEndian.Uint64(descriptor[8:16])
	length := binary.LittleEndian.Uint64(descriptor[16:24])
	if length == 0 && !backing.valid() {
		return "", true
	}
	if !backing.valid() || start > uint64(^uint32(0)) || length > uint64(^uint(0)>>1) {
		return "", false
	}
	text, err := heap.readString(backing, start, length)
	return text, err == nil
}

func materializeManagedDynamic(value vmValue) (vmValue, bool) {
	if value.heap == nil || !value.ref.valid() || value.layouts == nil {
		return vmValue{}, false
	}
	var raw [24]byte
	if err := value.heap.read(value.ref, value.offset, raw[:]); err != nil {
		return vmValue{}, false
	}
	encoded, err := decodeManagedDynamicWithRegistry(raw[:], value.Type, value.heap, value.layouts)
	if err != nil || encoded.representation == managedValueEmpty {
		return vmValue{}, false
	}
	return decodeManagedValueWithLayouts(encoded, value.heap, value.layouts, value.owner), true
}

func materializeManagedSlice(value vmValue) (*vmSlice, bool) {
	if value.heap == nil || !value.ref.valid() {
		return nil, false
	}
	return materializeManagedSliceAt(value.heap, value.ref, value.offset, value.Type, value.layouts, value.owner)
}

func materializeManagedSliceAt(heap *managedHeap, ref guestRef, offset uint32, typ vmType, registry *preparedLayoutRegistry, owner *vm) (*vmSlice, bool) {
	if heap == nil || !ref.valid() {
		return nil, false
	}
	element, ok := typ.SliceElem()
	if !ok || registry == nil {
		return nil, false
	}
	if owner != nil {
		element = owner.rootModule().resolvedRuntimeType(element)
	}
	var descriptor [32]byte
	if err := heap.read(ref, offset, descriptor[:]); err != nil {
		return nil, false
	}
	backing := guestRef{slot: binary.LittleEndian.Uint32(descriptor[0:4]), generation: binary.LittleEndian.Uint32(descriptor[4:8])}
	start := binary.LittleEndian.Uint64(descriptor[8:16])
	length := binary.LittleEndian.Uint64(descriptor[16:24])
	capacity := binary.LittleEndian.Uint64(descriptor[24:32])
	if !backing.valid() {
		return nil, length == 0 && capacity == 0
	}
	layout, err := registry.layoutFor(element)
	if err != nil {
		return nil, false
	}
	view := &vmSlice{
		managedBase:    uint32(start),
		Len:            int(length),
		Cap:            int(capacity),
		managedHeap:    heap,
		managedRef:     backing,
		managedType:    element,
		managedElem:    layout,
		managedByte:    element.Primitive(types.PrimitiveUint8),
		managedLayouts: registry,
		managedOwner:   owner,
	}
	return view, true
}

func materializeManagedArray(value vmValue) (*vmArray, bool) {
	if value.heap == nil || !value.ref.valid() || value.Type.ShapeKind() != types.Array {
		return nil, false
	}
	length, element, ok := value.Type.ArrayInfo()
	if !ok || length > uint64(^uint(0)>>1) || value.layouts == nil {
		return nil, false
	}
	if value.owner != nil {
		element = value.owner.rootModule().resolvedRuntimeType(element)
	}
	layout, err := value.layouts.layoutFor(element)
	if err != nil {
		return nil, false
	}
	return &vmArray{vmSlice: vmSlice{
		Start: 0, Len: int(length), Cap: int(length),
		managedHeap: value.heap, managedRef: value.ref, managedBase: value.offset,
		managedType: element, managedElem: layout,
		managedLayouts: value.layouts, managedOwner: value.owner,
	}}, true
}

func materializeManagedStruct(value vmValue) (*vmStruct, bool) {
	if value.heap == nil || !value.ref.valid() || value.Type.ShapeKind() != types.Struct {
		return nil, false
	}
	_, ok := value.Type.StructFields()
	if !ok || value.layouts == nil {
		return nil, false
	}
	return managedStructView(value.Type, value.heap, value.ref, value.offset, value.layouts, value.owner)
}

func managedStructView(typ vmType, heap *managedHeap, ref guestRef, base uint32, layouts *preparedLayoutRegistry, owner *vm) (*vmStruct, bool) {
	if layouts == nil {
		return nil, false
	}
	plan, err := layouts.structLayout(typ)
	if err != nil {
		return nil, false
	}
	return &vmStruct{schema: plan.schema, managedHeap: heap, managedRef: ref, managedFields: plan.fields, layouts: layouts, managedBase: base, owner: owner}, true
}

func readManagedSliceElement(slice *vmSlice, index int) vmValue {
	if slice == nil || index < 0 || index >= slice.Len || slice.managedHeap == nil {
		return vmValue{}
	}
	physical := slice.Start + index
	if slice.managedByte {
		var byteValue [1]byte
		if err := slice.managedHeap.read(slice.managedRef, slice.managedBase+uint32(physical), byteValue[:]); err != nil {
			return vmValue{}
		}
		return newVMValue(slice.managedType, uint64(byteValue[0]))
	}
	return decodeManagedInline(slice.managedHeap, slice.managedRef, slice.managedBase+uint32(physical)*slice.managedElem.Stride, slice.managedType, slice.managedElem, slice.managedLayouts, slice.managedOwner)
}

func writeManagedSliceElement(slice *vmSlice, index int, value vmValue) error {
	if slice == nil || index < 0 || index >= slice.Len || slice.managedHeap == nil {
		return errors.New("managed slice index out of range")
	}
	physical := slice.Start + index
	if slice.managedByte {
		number, err := numericAsUint64(value)
		if err != nil {
			return err
		}
		return slice.managedHeap.write(slice.managedRef, slice.managedBase+uint32(physical), []byte{byte(number)})
	}
	return writeManagedInlineAt(slice.managedHeap, slice.managedRef, slice.managedBase+uint32(physical)*slice.managedElem.Stride, slice.managedType, slice.managedElem, slice.managedLayouts, slice.managedOwner, value)
}

func writeManagedStructField(structure *vmStruct, index int, value vmValue) error {
	if structure == nil || structure.managedHeap == nil || index < 0 || index >= len(structure.managedFields) {
		return errors.New("managed struct field is unavailable")
	}
	field := structure.managedFields[index]
	return writeManagedInlineAt(structure.managedHeap, structure.managedRef, structure.managedBase+field.offset, field.typ, field.layout, structure.layouts, structure.owner, value)
}

func writeManagedInlineAt(heap *managedHeap, ref guestRef, offset uint32, typ vmType, layout preparedValueLayout, layouts *preparedLayoutRegistry, owner *vm, value vmValue) error {
	shape := layout.Shape
	primitive := layout.Primitive
	numericKind := layout.NumericKind
	if shape == types.Invalid {
		shape = typ.ShapeKind()
		if underlying := typ.Underlying(); underlying.Ref.Kind == types.Primitive {
			primitive = underlying.Ref.Primitive
		}
		if info, ok := typ.NumericInfo(); ok {
			numericKind = info.Kind
		}
	}
	if numericKind != types.NumericInvalid || primitive == types.PrimitiveBool {
		return writeManagedScalar(heap, ref, offset, value, layout.Stride)
	}
	if shape == types.Any || shape == types.Interface {
		if layouts == nil {
			return errors.New("dynamic managed value has no layout registry")
		}
		module := value.module
		if module == nil && owner != nil {
			module = owner.rootModule()
		}
		if module == nil {
			return errors.New("dynamic managed value has no module owner")
		}
		encoded, err := module.dynamicEncoding(value, map[uintptr]guestRef{})
		if err != nil {
			return err
		}
		dynamicLayout, err := layouts.layoutFor(encoded.typ)
		if err != nil {
			return err
		}
		var raw [24]byte
		if err := encodeManagedDynamic(raw[:], dynamicLayout.ID, encoded); err != nil {
			return err
		}
		return heap.write(ref, offset, raw[:min(len(raw), int(layout.Stride))])
	}
	if primitive == types.PrimitiveString || shape == types.Slice || shape == types.Array || shape == types.Struct {
		if value.ref.valid() && value.heap == heap {
			return heap.copy(ref, offset, value.ref, value.offset, layout.Stride)
		}
		if owner == nil {
			return errors.New("managed aggregate write has no owner")
		}
		module := owner.rootModule()
		if module == nil {
			module = &moduleInstance{vm: owner}
		}
		var scratch [maxInlineAggregateBytes]byte
		raw := scratch[:min(len(scratch), int(layout.Stride))]
		if int(layout.Stride) > len(scratch) {
			raw = make([]byte, layout.Stride)
		}
		if err := module.initializeManagedValue(raw, layout, value, map[uintptr]guestRef{}); err != nil {
			return err
		}
		return heap.write(ref, offset, raw)
	}
	encoded, err := encodeManagedWriteValue(owner, value)
	if err != nil {
		return err
	}
	if shape == types.Pointer {
		if !encoded.ref.valid() {
			return heap.clear(ref, offset, layout.Stride)
		}
		targetType, _ := typ.PointerElem()
		if owner != nil && owner.rootModule() != nil {
			targetType = owner.rootModule().resolvedRuntimeType(targetType)
		} else if resolved, ok := layouts.resolvedType(targetType); ok {
			targetType = resolved
		}
		targetLayout, err := layouts.layoutFor(targetType)
		if err != nil {
			return err
		}
		var raw [24]byte
		binary.LittleEndian.PutUint32(raw[:], encoded.ref.slot)
		binary.LittleEndian.PutUint32(raw[4:], encoded.ref.generation)
		binary.LittleEndian.PutUint64(raw[8:], encoded.hi)
		binary.LittleEndian.PutUint32(raw[16:], targetLayout.ID)
		return heap.write(ref, offset, raw[:])
	}
	if encoded.representation == managedValueRef || encoded.representation == managedValueBox {
		return heap.writeReference(ref, offset, encoded.ref)
	}
	if len(layout.References) != 0 {
		return heap.writeReference(ref, offset+layout.References[0], guestRef{})
	}
	var raw [16]byte
	if err := encodeScalarInto(raw[:], value); err != nil {
		return err
	}
	return heap.write(ref, offset, raw[:min(len(raw), int(layout.Stride))])
}

func encodeManagedWriteValue(owner *vm, value vmValue) (managedValueEncoding, error) {
	encoded, err := encodeManagedValue(value)
	if err == nil || owner == nil {
		return encoded, err
	}
	module := owner.rootModule()
	if module == nil {
		return managedValueEncoding{}, err
	}
	child, importErr := module.importDetachedValue(value, map[uintptr]guestRef{})
	if importErr != nil {
		return managedValueEncoding{}, importErr
	}
	return encodeManagedValue(child)
}

func decodeManagedInline(heap *managedHeap, ref guestRef, offset uint32, typ vmType, layout preparedValueLayout, layouts *preparedLayoutRegistry, owner *vm) vmValue {
	if heap == nil {
		return vmValue{Type: typ}
	}
	shape := layout.Shape
	primitive := layout.Primitive
	numericKind, numericBits := layout.NumericKind, layout.NumericBits
	if shape == types.Invalid {
		shape = typ.ShapeKind()
		if underlying := typ.Underlying(); underlying.Ref.Kind == types.Primitive {
			primitive = underlying.Ref.Primitive
		}
		if info, ok := typ.NumericInfo(); ok {
			numericKind, numericBits = info.Kind, uint16(info.Bits)
		}
	}
	if primitive == types.PrimitiveBool {
		var b [1]byte
		if heap.read(ref, offset, b[:]) != nil {
			return vmValue{Type: typ}
		}
		return newVMValue(typ, b[0] != 0)
	}
	if numericKind != types.NumericInvalid {
		var raw [16]byte
		width := int(numericBits) / 8
		if width == 0 {
			width = 8
		}
		if heap.read(ref, offset, raw[:width]) != nil {
			return vmValue{Type: typ}
		}
		return decodeScalarBytesWithInfo(typ, raw[:width], types.NumericInfo{
			Primitive: primitive,
			Kind:      numericKind,
			Bits:      int(numericBits),
		}, true)
	}
	if primitive == types.PrimitiveString {
		if text, ok := materializeManagedStringAt(heap, ref, offset); ok {
			return vmValue{Type: typ, Data: text, heap: heap, owner: owner, layouts: layouts}
		}
		return vmValue{Type: typ, heap: heap, owner: owner, layouts: layouts}
	}
	if shape == types.Slice {
		if slice, ok := materializeManagedSliceAt(heap, ref, offset, typ, layouts, owner); ok {
			return vmValue{Type: typ, Data: slice, heap: heap, owner: owner, layouts: layouts}
		}
		return vmValue{Type: typ, heap: heap, owner: owner, layouts: layouts}
	}
	if shape == types.Struct {
		return vmValue{Type: typ, ref: ref, offset: offset, heap: heap, owner: owner, layouts: layouts}
	}
	if shape == types.Array {
		return vmValue{Type: typ, ref: ref, offset: offset, heap: heap, owner: owner, layouts: layouts}
	}
	if shape == types.Pointer {
		var raw [24]byte
		if err := heap.read(ref, offset, raw[:]); err == nil {
			target := guestRef{slot: binary.LittleEndian.Uint32(raw[:]), generation: binary.LittleEndian.Uint32(raw[4:])}
			location := binary.LittleEndian.Uint64(raw[8:])
			if target.valid() && location <= math.MaxUint32 {
				return vmValue{Type: typ, ref: target, offset: uint32(location), heap: heap, owner: owner, layouts: layouts}
			}
		}
		return vmValue{Type: typ}
	}
	if len(layout.References) != 0 {
		child, err := heap.readReference(ref, offset+layout.References[0])
		if err == nil {
			return vmValue{Type: typ, ref: child, heap: heap, owner: owner, layouts: layouts}
		}
	}
	if len(layout.Dynamics) != 0 {
		return vmValue{Type: typ, ref: ref, offset: offset + layout.Dynamics[0], heap: heap, owner: owner, layouts: layouts}
	}
	return vmValue{Type: typ}
}

func encodeScalarInto(dst []byte, value vmValue) error {
	if len(dst) < 8 {
		return errors.New("scalar buffer is too small")
	}
	return writeScalarBytes(dst, 0, value)
}

func decodeScalarBytes(typ vmType, raw []byte) vmValue {
	info, ok := typ.NumericInfo()
	return decodeScalarBytesWithInfo(typ, raw, info, ok)
}

func decodeScalarBytesWithInfo(typ vmType, raw []byte, info types.NumericInfo, numeric bool) vmValue {
	var word [8]byte
	copy(word[:], raw)
	bits := binary.LittleEndian.Uint64(word[:])
	if numeric {
		switch info.Kind {
		case types.NumericSigned:
			return normalizeSignedValue(typ, int64(bits))
		case types.NumericUnsigned:
			return normalizeUnsignedValue(typ, bits)
		case types.NumericFloat:
			if info.Bits == 32 {
				return normalizeFloatValue(typ, float64(math.Float32frombits(uint32(bits))))
			}
			return normalizeFloatValue(typ, math.Float64frombits(bits))
		case types.NumericComplex:
			componentWidth := info.Bits / 16
			if componentWidth == 0 {
				componentWidth = 4
			}
			if len(raw) < componentWidth*2 {
				return vmValue{Type: typ}
			}
			var high [8]byte
			copy(high[:], raw[componentWidth:componentWidth*2])
			if componentWidth == 4 {
				return normalizeComplexValue(typ, complex(float64(math.Float32frombits(uint32(bits))), float64(math.Float32frombits(uint32(binary.LittleEndian.Uint64(high[:]))))))
			}
			return normalizeComplexValue(typ, complex(math.Float64frombits(bits), math.Float64frombits(binary.LittleEndian.Uint64(high[:]))))
		}
	}
	return vmValue{Type: typ, scalar: bits, Data: vmScalarData}
}

func managedDetachedIdentity(payload any) (uintptr, bool) {
	switch data := payload.(type) {
	case *vmArray:
		return uintptrPointer(data)
	case *vmSlice:
		return uintptrPointer(data)
	case *vmStruct:
		return uintptrPointer(data)
	case *vmMap:
		return uintptrPointer(data)
	case *vmPointer:
		return uintptrPointer(data)
	default:
		return 0, false
	}
}

func uintptrPointer(value any) (uintptr, bool) {
	if value == nil {
		return 0, false
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		pointer := reflected.Pointer()
		return pointer, pointer != 0
	default:
		return 0, false
	}
}

func encodeManagedValue(value vmValue) (managedValueEncoding, error) {
	encoded := managedValueEncoding{typ: value.Type}
	if value.scalarKind != 0 {
		encoded.representation = managedValueBits
		encoded.lo = value.scalar
		return encoded, nil
	}
	if boolean, ok := value.boolValue(); ok {
		encoded.representation = managedValueBits
		if boolean {
			encoded.lo = 1
		}
		return encoded, nil
	}
	if value.ref.valid() {
		encoded.representation = managedValueRef
		encoded.ref = value.ref
		encoded.hi = uint64(value.offset)
		return encoded, nil
	}
	if isNilRuntimeValue(value) {
		encoded.representation = managedValueTypedNil
		return encoded, nil
	}
	if complexValue, ok := value.Data.(complex128); ok {
		encoded.representation = managedValueBits
		encoded.lo = math.Float64bits(real(complexValue))
		encoded.hi = math.Float64bits(imag(complexValue))
		return encoded, nil
	}
	return managedValueEncoding{}, errors.New("managed value requires a published guest reference")
}

func decodeManagedValueWithLayouts(encoded managedValueEncoding, heap *managedHeap, layouts *preparedLayoutRegistry, owner *vm) vmValue {
	value := vmValue{Type: encoded.typ}
	value.owner, value.layouts = owner, layouts
	switch encoded.representation {
	case managedValueBits:
		if encoded.typ.Primitive(types.PrimitiveBool) {
			value.scalar = encoded.lo
			value.Data = encoded.lo != 0
			return value
		}
		value.scalar = encoded.lo
		if kind := scalarKindForType(encoded.typ); kind != 0 {
			value.Data = vmScalarData
			value.scalarKind = kind
			return value
		}
		value.Data = complex(math.Float64frombits(encoded.lo), math.Float64frombits(encoded.hi))
	case managedValueRef, managedValueBox:
		value.ref = encoded.ref
		value.offset = uint32(encoded.hi)
		value.heap = heap
	case managedValueTypedNil, managedValueTypedZero, managedValueEmpty:
		// Keep Data nil. A typed nil/zero is identified by Type and the
		// representation, not by a host object allocated for the value.
	}
	return value
}

// encodeManagedDynamic writes the common 24-byte dynamic slot. typeID is a
// prepared TypeTable ordinal; it must be resolved before execution and is not
// inferred by scanning a Go interface value.
func encodeManagedDynamic(dst []byte, typeID uint32, encoded managedValueEncoding) error {
	if len(dst) < 24 {
		return errors.New("dynamic slot is smaller than 24 bytes")
	}
	clear(dst[:24])
	if encoded.representation == managedValueEmpty {
		return nil
	}
	binary.LittleEndian.PutUint32(dst[0:4], typeID)
	binary.LittleEndian.PutUint32(dst[4:8], uint32(encoded.representation))
	binary.LittleEndian.PutUint64(dst[8:16], encoded.lo)
	binary.LittleEndian.PutUint64(dst[16:24], encoded.hi)
	if encoded.representation == managedValueRef || encoded.representation == managedValueBox {
		binary.LittleEndian.PutUint32(dst[8:12], encoded.ref.slot)
		binary.LittleEndian.PutUint32(dst[12:16], encoded.ref.generation)
	}
	return nil
}

func decodeManagedDynamicWithRegistry(src []byte, typ vmType, heap *managedHeap, layouts *preparedLayoutRegistry) (managedValueEncoding, error) {
	if len(src) < 24 {
		return managedValueEncoding{}, errors.New("dynamic slot is smaller than 24 bytes")
	}
	encoded := managedValueEncoding{
		typ:            typ,
		representation: managedValueRepresentation(binary.LittleEndian.Uint32(src[4:8])),
		lo:             binary.LittleEndian.Uint64(src[8:16]),
		hi:             binary.LittleEndian.Uint64(src[16:24]),
	}
	if layouts != nil {
		if concrete, ok := layouts.typeByID(binary.LittleEndian.Uint32(src[0:4])); ok {
			encoded.typ = concrete
		}
	}
	if encoded.representation == managedValueRef || encoded.representation == managedValueBox {
		encoded.ref = guestRef{slot: binary.LittleEndian.Uint32(src[8:12]), generation: binary.LittleEndian.Uint32(src[12:16])}
	}
	_ = heap
	return encoded, nil
}

// readManagedScalar and writeManagedScalar are the hot scalar page accessors.
// They intentionally operate on a caller-provided offset and never expose the
// page byte slice across a safepoint.
func readManagedScalar(heap *managedHeap, ref guestRef, offset uint32, typ vmType) (vmValue, error) {
	info, _ := typ.NumericInfo()
	width := 8
	if info.Bits > 0 {
		width = info.Bits / 8
	}
	if typ.Primitive(types.PrimitiveBool) {
		width = 1
	}
	if width < 1 {
		width = 1
	}
	var raw [16]byte
	if err := heap.read(ref, offset, raw[:width]); err != nil {
		return vmValue{}, err
	}
	if typ.Primitive(types.PrimitiveBool) {
		return newVMValue(typ, raw[0] != 0), nil
	}
	return decodeScalarBytes(typ, raw[:width]), nil
}

func writeManagedScalar(heap *managedHeap, ref guestRef, offset uint32, value vmValue, width uint32) error {
	if width == 0 {
		width = 8
		if info, ok := value.Type.NumericInfo(); ok {
			width = uint32(info.Bits / 8)
		} else if value.Type.Primitive(types.PrimitiveBool) {
			width = 1
		}
	}
	if width < 1 {
		width = 1
	}
	var raw [16]byte
	if err := writeScalarBytes(raw[:width], 0, value); err != nil {
		return err
	}
	return heap.write(ref, offset, raw[:width])
}

func scalarKindForType(typ vmType) vmScalarKind {
	if info, ok := typ.NumericInfo(); ok {
		switch info.Kind {
		case types.NumericSigned:
			return vmScalarSigned
		case types.NumericUnsigned:
			return vmScalarUnsigned
		case types.NumericFloat:
			return vmScalarFloat
		}
	}
	return 0
}
