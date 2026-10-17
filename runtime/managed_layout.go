package runtime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"unsafe"
	"weak"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// preparedValueLayout is the runtime physical layout used by the value codec.
// Offsets are byte offsets within one value, and are independent of the
// language-visible reflect layout. The heap converts this descriptor to its
// page layout once the value is admitted.
type preparedValueLayout struct {
	ID      uint32
	Stride  uint32
	Logical uint64
	// Prepared means the physical descriptor was built from a validated type.
	// Loader-owned descriptors have no registry ID yet, but are still complete
	// enough for frame-local reads and writes. The child pointers keep aggregate
	// access on that descriptor instead of returning to the registry.
	Prepared     bool
	ArrayElement *preparedValueLayout
	SliceElement *preparedValueLayout
	StructPlan   *preparedStructLayout
	// Classification is prepared with the physical layout so frame and
	// managed-value access does not repeatedly resolve the type table. Primitive
	// is the underlying primitive for named values; NumericKind is Invalid for
	// non-numeric values.
	Shape       types.Kind
	Primitive   types.PrimitiveKind
	NumericKind types.NumericKind
	NumericBits uint16
	Fingerprint [sha256.Size]byte
	References  []uint32
	Dynamics    []uint32
	TypeOffsets []uint32
}

type preparedLayoutRegistry struct {
	mu            sync.RWMutex
	nextID        uint32
	byType        map[string]preparedValueLayout
	layouts       map[uint32]preparedValueLayout
	types         map[uint32]vmType
	verified      map[string]preparedTypeVerification
	marked        map[uint32]uint64
	resident      map[uint32]uint64
	mapLayouts    map[uint32]*preparedMapLayout
	structLayouts map[uint32]*preparedStructLayout
	heap          *managedHeap
	owner         *vm
}

type preparedStructLayout struct {
	schema *structSchema
	fields []managedStructField
	marked uint64
}

var errRecursivePreparedLayout = errors.New("recursive value layout")

type preparedTypeVerification struct {
	tables [4]weak.Pointer[types.TypeTable]
	refs   [4]types.TypeRef
	next   uint8
}

func (verification preparedTypeVerification) remember(typ vmType) preparedTypeVerification {
	index := verification.next % uint8(len(verification.tables))
	verification.tables[index], verification.refs[index] = weak.Make(typ.Table), typ.Ref
	verification.next = index + 1
	return verification
}

func newPreparedLayoutRegistry() *preparedLayoutRegistry {
	return &preparedLayoutRegistry{nextID: 1, byType: make(map[string]preparedValueLayout), layouts: make(map[uint32]preparedValueLayout), types: make(map[uint32]vmType), verified: make(map[string]preparedTypeVerification), marked: make(map[uint32]uint64), resident: make(map[uint32]uint64)}
}

func (registry *preparedLayoutRegistry) load(typ vmType) (preparedValueLayout, bool) {
	if registry == nil {
		return preparedValueLayout{}, false
	}
	registry.mu.RLock()
	key := typ.String()
	layout, ok := registry.byType[key]
	verified := registry.verified[key]
	registry.mu.RUnlock()
	if ok {
		// Primitive representation is independent of the source table. Values
		// read from different owned type closures share this same descriptor.
		if typ.Ref.Kind == types.Primitive || typ.Ref.Kind == types.Any || typ.Ref.Kind == types.Void {
			return layout, true
		}
		for index, table := range verified.tables {
			if table.Value() == typ.Table && verified.refs[index] == typ.Ref {
				return layout, true
			}
		}
	}
	return layout, false
}

func (registry *preparedLayoutRegistry) layoutFor(typ vmType) (preparedValueLayout, error) {
	if !typ.Valid() {
		return preparedValueLayout{}, errors.New("cannot prepare layout for invalid type")
	}
	if layout, ok := registry.load(typ); ok {
		return layout, nil
	}
	key := typ.String()
	var resolve func(any) vmType
	if registry.owner != nil {
		if module := registry.owner.rootModule(); module != nil {
			resolve = module.resolvedRuntimeType
		}
	}
	layout, err := buildPreparedValueLayout(typ, map[string]bool{}, resolve)
	if err != nil {
		return preparedValueLayout{}, err
	}
	registry.mu.Lock()
	if existing, ok := registry.byType[key]; ok {
		if !samePreparedLayout(existing, layout) {
			registry.mu.Unlock()
			return preparedValueLayout{}, fmt.Errorf("storage layout changed for type %s", key)
		}
		registry.verified[key] = registry.verified[key].remember(typ)
		registry.mu.Unlock()
		return existing, nil
	}
	registry.mu.Unlock()
	dataType, err := ownedStorageType(typ, resolve)
	if err != nil {
		return preparedValueLayout{}, err
	}
	// The registry owns the prepared descriptor and must not retain the
	// program's type table through child vmType/struct-plan metadata. Rebuild
	// the immutable plan against the minimal owned closure before publishing it.
	ownedLayout, err := buildPreparedValueLayout(dataType, map[string]bool{}, nil)
	if err != nil {
		return preparedValueLayout{}, err
	}
	if !samePreparedLayout(layout, ownedLayout) {
		return preparedValueLayout{}, fmt.Errorf("owned storage layout changed for type %s", key)
	}
	layout.ArrayElement = ownedLayout.ArrayElement
	layout.SliceElement = ownedLayout.SliceElement
	layout.StructPlan = ownedLayout.StructPlan
	// The budget covers the storage we control: descriptors, offset arrays and
	// owned type closure. Host map buckets and allocator overhead remain host
	// memory, rather than pretending to be exact guest residency.
	resident := preparedLayoutResident(layout) + uint64(unsafe.Sizeof(vmType{})+unsafe.Sizeof(preparedTypeVerification{})) + uint64(len(key))
	if dataType.Table != nil {
		owned := dataType.Table
		resident += uint64(unsafe.Sizeof(types.TypeTable{})) + uint64(cap(owned.Nodes))*uint64(unsafe.Sizeof(types.TypeNode{}))
		for _, node := range owned.Nodes {
			resident += uint64(cap(node.Fields))*uint64(unsafe.Sizeof(types.Field{})) + uint64(cap(node.Methods))*uint64(unsafe.Sizeof(types.Method{}))
			resident += uint64(cap(node.Tuple)+cap(node.TypeArgs))*uint64(unsafe.Sizeof(types.TypeRef{})) + uint64(cap(node.Terms))*uint64(unsafe.Sizeof(types.TypeTerm{}))
			if node.Signature != nil {
				resident += uint64(unsafe.Sizeof(types.FunctionSignature{})) + uint64(cap(node.Signature.Params))*uint64(unsafe.Sizeof(types.TypeParam{})) + uint64(cap(node.Signature.Results))*uint64(unsafe.Sizeof(types.TypeRef{}))
			}
			for _, method := range node.Methods {
				resident += uint64(cap(method.Signature.Params))*uint64(unsafe.Sizeof(types.TypeParam{})) + uint64(cap(method.Signature.Results))*uint64(unsafe.Sizeof(types.TypeRef{}))
			}
		}
	}
	if registry.heap != nil {
		registry.heap.mu.Lock()
		defer registry.heap.mu.Unlock()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.byType == nil {
		registry.byType = make(map[string]preparedValueLayout)
	}
	if registry.layouts == nil {
		registry.layouts = make(map[uint32]preparedValueLayout)
	}
	if registry.types == nil {
		registry.types = make(map[uint32]vmType)
	}
	if registry.verified == nil {
		registry.verified = make(map[string]preparedTypeVerification)
	}
	if registry.nextID == 0 {
		registry.nextID = 1
	}
	if existing, ok := registry.byType[key]; ok {
		if !samePreparedLayout(existing, layout) {
			return preparedValueLayout{}, fmt.Errorf("storage layout changed for type %s", key)
		}
		registry.verified[key] = registry.verified[key].remember(typ)
		return existing, nil
	}
	if registry.heap != nil && !registry.heap.admitResident(resident) {
		return preparedValueLayout{}, errManagedLimit
	}
	layout.ID = registry.nextID
	registry.nextID++
	registry.byType[key] = layout
	registry.layouts[layout.ID] = layout
	registry.types[layout.ID] = dataType
	if registry.heap != nil {
		registry.resident[layout.ID] = resident
	}
	registry.verified[key] = registry.verified[key].remember(typ)
	return layout, nil
}

// preparedLayoutResident includes the immutable child descriptors constructed
// by the loader. They are owned by the root registry entry and must be
// released together when that entry is swept; counting only the root made the
// managed resident budget systematically understate aggregate-heavy layouts.
func preparedLayoutResident(layout preparedValueLayout) uint64 {
	seenLayouts := make(map[*preparedValueLayout]struct{})
	seenPlans := make(map[*preparedStructLayout]struct{})
	var visitLayout func(*preparedValueLayout, bool) uint64
	visitPlan := func(plan *preparedStructLayout) uint64 {
		if plan == nil {
			return 0
		}
		if _, ok := seenPlans[plan]; ok {
			return 0
		}
		seenPlans[plan] = struct{}{}
		resident := uint64(unsafe.Sizeof(preparedStructLayout{})) + uint64(cap(plan.fields))*uint64(unsafe.Sizeof(managedStructField{}))
		if plan.schema != nil {
			resident += uint64(unsafe.Sizeof(structSchema{})) + uint64(cap(plan.schema.fields))*uint64(unsafe.Sizeof(TypeFieldInfo{}))
		}
		for index := range plan.fields {
			resident += visitLayout(&plan.fields[index].layout, false)
		}
		return resident
	}
	visitLayout = func(current *preparedValueLayout, countSelf bool) uint64 {
		if current == nil {
			return 0
		}
		if _, ok := seenLayouts[current]; ok {
			return 0
		}
		seenLayouts[current] = struct{}{}
		resident := uint64((len(current.References) + len(current.Dynamics) + len(current.TypeOffsets)) * 4)
		if countSelf {
			resident += uint64(unsafe.Sizeof(preparedValueLayout{}))
		}
		resident += visitLayout(current.ArrayElement, true) + visitLayout(current.SliceElement, true)
		resident += visitPlan(current.StructPlan)
		return resident
	}
	return visitLayout(&layout, true)
}

func samePreparedLayout(left, right preparedValueLayout) bool {
	return left.Stride == right.Stride && left.Logical == right.Logical && left.Shape == right.Shape && left.Primitive == right.Primitive && left.NumericKind == right.NumericKind && left.NumericBits == right.NumericBits && left.Fingerprint == right.Fingerprint && slices.Equal(left.References, right.References) && slices.Equal(left.Dynamics, right.Dynamics) && slices.Equal(left.TypeOffsets, right.TypeOffsets)
}

func (registry *preparedLayoutRegistry) mark(id uint32, epoch uint64) {
	if id == 0 {
		return
	}
	registry.mu.Lock()
	if _, ok := registry.layouts[id]; ok {
		registry.marked[id] = epoch
	}
	registry.mu.Unlock()
}

func (registry *preparedLayoutRegistry) markPreparedTypes(keys []string, epoch uint64) {
	registry.mu.Lock()
	for _, key := range keys {
		if layout, ok := registry.byType[key]; ok {
			registry.marked[layout.ID] = epoch
		}
	}
	registry.mu.Unlock()
}

// sweep follows a completed major mark. Code marks its prepared static keys;
// numeric dynamic/pointer type IDs are marked directly from live payloads.
func (registry *preparedLayoutRegistry) sweep(epoch uint64) uint64 {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	var released uint64
	for id := range registry.mapLayouts {
		if registry.marked[id] == epoch {
			registry.markLayoutDependencies(id, epoch)
		}
	}
	for id := range registry.structLayouts {
		if registry.marked[id] == epoch {
			registry.markLayoutDependencies(id, epoch)
		}
	}
	for key, layout := range registry.byType {
		if registry.marked[layout.ID] == epoch {
			continue
		}
		released += registry.resident[layout.ID]
		delete(registry.byType, key)
		delete(registry.verified, key)
		delete(registry.types, layout.ID)
		delete(registry.layouts, layout.ID)
		delete(registry.marked, layout.ID)
		delete(registry.resident, layout.ID)
		delete(registry.mapLayouts, layout.ID)
		delete(registry.structLayouts, layout.ID)
	}
	return released
}

// Prepared containers own their child descriptors even when empty. Follow
// those cache dependencies transitively, independently of Go's
// map iteration order and without allocating collector work storage.
func (registry *preparedLayoutRegistry) markLayoutDependencies(id uint32, epoch uint64) {
	if layout := registry.mapLayouts[id]; layout != nil && layout.marked != epoch {
		layout.marked = epoch
		for _, child := range [2]uint32{layout.keyLayout.ID, layout.valueLayout.ID} {
			registry.marked[child] = epoch
			registry.markLayoutDependencies(child, epoch)
		}
	}
	if layout := registry.structLayouts[id]; layout != nil && layout.marked != epoch {
		layout.marked = epoch
		for _, field := range layout.fields {
			registry.marked[field.layout.ID] = epoch
			registry.markLayoutDependencies(field.layout.ID, epoch)
		}
	}
}

// structLayout owns field offsets alongside the parent layout. A temporary
// struct view borrows this immutable plan instead of rebuilding schemas and
// resolving every child type on each field access.
func (registry *preparedLayoutRegistry) structLayout(typ vmType) (*preparedStructLayout, error) {
	parent, err := registry.layoutFor(typ)
	if err != nil {
		return nil, err
	}
	registry.mu.RLock()
	existing := registry.structLayouts[parent.ID]
	owned := registry.types[parent.ID]
	registry.mu.RUnlock()
	if existing != nil {
		return existing, nil
	}
	fields, ok := owned.StructFields()
	if !ok {
		return nil, errors.New("managed struct fields are unavailable")
	}
	info := make([]TypeFieldInfo, 0, len(fields))
	prepared := make([]managedStructField, 0, len(fields))
	var sourceSchema *structSchema
	if registry.owner != nil {
		if module := registry.owner.rootModule(); module != nil {
			sourceSchema, _ = module.structSchema(typ)
		}
	}
	var offset uint32
	for _, field := range fields {
		if remainder := offset % 8; remainder != 0 {
			offset += 8 - remainder
		}
		fieldType := owned.derived(field.Type)
		layout, err := registry.layoutFor(fieldType)
		if err != nil {
			return nil, err
		}
		if field.Name != "" && field.Name != "_" {
			metadata := TypeFieldInfo{Name: field.Name, RuntimeType: fieldType, Tag: field.Tag, Embedded: field.Embedded, Exported: isExportedName(field.Name)}
			if _, original, found := sourceSchema.field(field.Name); found {
				metadata = original
				metadata.RuntimeType = fieldType
			}
			info = append(info, metadata)
			prepared = append(prepared, managedStructField{offset: offset, typ: fieldType, layout: layout})
		}
		offset += layout.Stride
	}
	plan := &preparedStructLayout{schema: newStructSchema(owned, info), fields: prepared}
	resident := uint64(unsafe.Sizeof(preparedStructLayout{})+unsafe.Sizeof(structSchema{})) +
		uint64(cap(plan.fields))*uint64(unsafe.Sizeof(managedStructField{})) +
		uint64(cap(plan.schema.fields))*uint64(unsafe.Sizeof(TypeFieldInfo{}))
	if registry.heap != nil {
		registry.heap.mu.Lock()
		defer registry.heap.mu.Unlock()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if existing := registry.structLayouts[parent.ID]; existing != nil {
		return existing, nil
	}
	if registry.heap != nil && !registry.heap.admitResident(resident) {
		return nil, errManagedLimit
	}
	if registry.structLayouts == nil {
		registry.structLayouts = make(map[uint32]*preparedStructLayout)
	}
	registry.structLayouts[parent.ID] = plan
	if registry.heap != nil {
		registry.resident[parent.ID] += resident
	}
	return plan, nil
}

func (registry *preparedLayoutRegistry) typeByID(id uint32) (vmType, bool) {
	if registry == nil {
		return vmType{}, false
	}
	registry.mu.RLock()
	typ, ok := registry.types[id]
	registry.mu.RUnlock()
	return typ, ok
}

func (registry *preparedLayoutRegistry) resolvedType(typ vmType) (vmType, bool) {
	if registry == nil {
		return vmType{}, false
	}
	registry.mu.RLock()
	layout, ok := registry.byType[typ.String()]
	resolved := registry.types[layout.ID]
	registry.mu.RUnlock()
	return resolved, ok && resolved.ShapeKind() != types.Named
}

func (registry *preparedLayoutRegistry) byID(id uint32) (preparedValueLayout, bool) {
	registry.mu.RLock()
	layout, ok := registry.layouts[id]
	registry.mu.RUnlock()
	return layout, ok
}

func preparedTypeKey(typ vmType) string {
	ref := typ.Ref
	return fmt.Sprintf("%d:%d:%s:%s:%s", ref.Kind, ref.Primitive, ref.Named.ModulePath, ref.Named.DeclID, ref.Node)
}

func preparedResolvedTypeKey(typ vmType, resolve func(any) vmType) string {
	if resolve != nil && typ.Ref.Kind == types.Named {
		resolved := resolve(typ)
		if resolved.Ref.Kind != types.Named {
			typ = resolved
		}
	}
	return preparedTypeKey(typ)
}

func buildPreparedValueLayout(typ vmType, active map[string]bool, resolve func(any) vmType) (layout preparedValueLayout, err error) {
	if resolve != nil && typ.Ref.Kind == types.Named {
		typ = resolve(typ)
	}
	key := preparedTypeKey(typ)
	if active[key] {
		return preparedValueLayout{}, fmt.Errorf("%w %q", errRecursivePreparedLayout, key)
	}
	active[key] = true
	defer delete(active, key)
	// Canonical shape excludes table-local node IDs and executable method
	// bodies. Inline child fingerprints also bind nested named types to their
	// actual data representation; equal widths alone cannot establish that
	// an old payload can be read using a new revision's field semantics.
	fingerprint := sha256.New()
	fingerprint.Write([]byte(typ.Underlying().String()))
	fingerprint.Write([]byte{0})
	defer func() { copy(layout.Fingerprint[:], fingerprint.Sum(nil)) }()
	layout = preparedValueLayout{
		Stride: ir.StorageScalarBytes, Logical: ir.StorageScalarBytes, Prepared: true,
		Shape: typ.ShapeKind(),
	}
	if underlying := typ.Underlying(); underlying.Ref.Kind == types.Primitive {
		layout.Primitive = underlying.Ref.Primitive
	}
	if info, ok := typ.NumericInfo(); ok {
		layout.NumericKind = info.Kind
		layout.NumericBits = uint16(info.Bits)
	}
	addReference := func(offset uint32) { layout.References = append(layout.References, offset) }
	addDynamic := func(offset uint32) {
		layout.Dynamics = append(layout.Dynamics, offset)
	}

	if typ.Primitive(types.PrimitiveBool) {
		layout.Stride, layout.Logical = ir.StorageByteBytes, ir.StorageByteBytes
		return layout, nil
	}
	if info, ok := typ.NumericInfo(); ok {
		layout.Stride = uint32(info.Bits / 8)
		layout.Logical = uint64(info.Bits / 8)
		if layout.Stride == 0 {
			layout.Stride, layout.Logical = 8, 8
		}
		return layout, nil
	}
	if typ.Primitive(types.PrimitiveString) {
		layout.Stride, layout.Logical = ir.StorageStringBytes, ir.StorageStringBytes
		addReference(0)
		return layout, nil
	}

	switch typ.ShapeKind() {
	case types.Any, types.Interface:
		layout.Stride, layout.Logical = ir.StorageDynamicBytes, ir.StorageDynamicBytes
		addDynamic(0)
	case types.Slice:
		layout.Stride, layout.Logical = ir.StorageSliceBytes, ir.StorageSliceBytes
		addReference(0)
		if element, ok := typ.SliceElem(); ok && !active[preparedResolvedTypeKey(element, resolve)] {
			preparedElement, childErr := buildPreparedValueLayout(element, active, resolve)
			if childErr != nil {
				if !errors.Is(childErr, errRecursivePreparedLayout) {
					return preparedValueLayout{}, childErr
				}
			} else {
				layout.SliceElement = &preparedElement
			}
		}
	case types.Pointer:
		layout.Stride, layout.Logical = ir.StoragePointerBytes, ir.StoragePointerBytes
		addReference(0)
		layout.TypeOffsets = append(layout.TypeOffsets, 16)
	case types.Map, types.Waitable, types.Function:
		layout.Stride, layout.Logical = ir.StorageReferenceBytes, ir.StorageReferenceBytes
		addReference(0)
	case types.Array:
		length, elem, ok := typ.ArrayInfo()
		if !ok {
			return preparedValueLayout{}, fmt.Errorf("array layout unavailable for %s", typ)
		}
		element, err := buildPreparedValueLayout(elem, active, resolve)
		if err != nil {
			return preparedValueLayout{}, err
		}
		fingerprint.Write(element.Fingerprint[:])
		physicalElementStride := element.Stride
		if physicalElementStride == 0 && length != 0 {
			// Keep a usable physical lane for repeated zero-width values while
			// preserving their zero logical cost. Physical page span and guest
			// accounting are intentionally separate.
			physicalElementStride = ir.StorageScalarBytes
		}
		if physicalElementStride != 0 && length > uint64(^uint32(0))/uint64(physicalElementStride) {
			return preparedValueLayout{}, fmt.Errorf("array layout overflows for %s", typ)
		}
		layout.ArrayElement = &element
		layout.Stride = uint32(length) * physicalElementStride
		layout.Logical = element.Logical * length
		if layout.Stride == 0 {
			layout.Stride = ir.StorageScalarBytes
		}
		for index := uint64(0); index < length; index++ {
			base := uint32(index) * physicalElementStride
			for _, offset := range element.References {
				addReference(base + offset)
			}
			for _, offset := range element.Dynamics {
				layout.Dynamics = append(layout.Dynamics, base+offset)
			}
			for _, offset := range element.TypeOffsets {
				layout.TypeOffsets = append(layout.TypeOffsets, base+offset)
			}
		}
	case types.Struct:
		fields, ok := typ.StructFields()
		if !ok {
			return preparedValueLayout{}, fmt.Errorf("struct layout unavailable for %s", typ)
		}
		var offset uint32
		layout.Logical = 0
		fieldInfo := make([]TypeFieldInfo, 0, len(fields))
		preparedFields := make([]managedStructField, 0, len(fields))
		for _, field := range fields {
			fieldType := typ.derived(field.Type)
			fieldLayout, err := buildPreparedValueLayout(fieldType, active, resolve)
			if err != nil {
				return preparedValueLayout{}, err
			}
			fingerprint.Write(fieldLayout.Fingerprint[:])
			if remainder := offset % 8; remainder != 0 {
				offset += 8 - remainder
			}
			if field.Name != "" && field.Name != "_" {
				fieldInfo = append(fieldInfo, TypeFieldInfo{
					Name: field.Name, RuntimeType: fieldType, Tag: field.Tag,
					Embedded: field.Embedded, Exported: isExportedName(field.Name),
				})
				preparedFields = append(preparedFields, managedStructField{offset: offset, typ: fieldType, layout: fieldLayout})
			}
			for _, refOffset := range fieldLayout.References {
				addReference(offset + refOffset)
			}
			for _, dynamicOffset := range fieldLayout.Dynamics {
				layout.Dynamics = append(layout.Dynamics, offset+dynamicOffset)
			}
			for _, typeOffset := range fieldLayout.TypeOffsets {
				layout.TypeOffsets = append(layout.TypeOffsets, offset+typeOffset)
			}
			offset += fieldLayout.Stride
			layout.Logical += fieldLayout.Logical
		}
		layout.Stride = offset
		if layout.Stride == 0 {
			// A zero-field struct has no inline guest payload. A standalone
			// record may still reserve the allocator's minimum physical lane,
			// but it remains free of logical value bytes.
			layout.Stride = ir.StorageScalarBytes
		}
		layout.StructPlan = &preparedStructLayout{schema: newStructSchema(typ, fieldInfo), fields: preparedFields}
	case types.Named:
		return preparedValueLayout{}, fmt.Errorf("unresolved storage type %s", typ)
	default:
		// Opaque runtime values use one reference lane. The side table owns
		// host state; the guest record still has a stable numeric identity.
		layout.Stride, layout.Logical = ir.StorageReferenceBytes, ir.StorageReferenceBytes
		addReference(0)
	}
	return layout, nil
}
