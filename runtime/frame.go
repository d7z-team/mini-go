package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type frame struct {
	execution          executionFrame
	allocationBatch    *managedAllocationBatch
	revision           *instanceRevision
	module             *moduleInstance
	function           loadedFunction
	shape              frameShapeIdentity
	executionContextID int64
	pc                 int
	typeDispatchIndex  int
	typeDispatchValue  vmValue
	typeDispatchActive bool
	localCells         []*slot
	localStorage       []slot
	// Small non-addressable values stay in the frame, including their reference
	// and dynamic lanes. Their bytes are the canonical value storage; escaped
	// cells retain independent managed identities.
	inlineStorage     []byte
	inlineOffsets     []uint32
	inlineLayouts     []preparedValueLayout
	inlineInitialized []bool
	// Temporaries use the same prepared byte layout as locals. Large aggregate
	// slots retain a managed handle; slotValues also holds operation values.
	tempStorage     []byte
	tempOffsets     []uint32
	tempLayouts     []preparedValueLayout
	tempTypes       []vmType
	tempInitialized []bool
	tempHeap        []bool
	// Scalar locals use typed lanes owned by the frame. Address-taken locals
	// continue to use stable cells; aggregate locals never enter this bank.
	directTypes       []vmType
	directScalars     []uint64
	directKinds       []vmScalarKind
	directInitialized []bool
	upvalueCells      []*slot
	slotValues        []vmValue
	slotOperands      *ir.RegisterFlow
	slotPC            int
	slotInput         int
	slotOutput        int
	popValues         []vmValue
	returnValues      []vmValue
	defers            []deferredCall
	// Prepared local indexes are the sole iterator owner. Diagnostics project
	// the local name from function metadata instead of maintaining a second
	// string keyed map in every activation.
	mapIteratorSlots   []*mapIterator
	idlePhysicalBytes  int64
	residentBytes      uint64
	residentHeap       *managedHeap
	allocationAdmitted bool
	frameLogicalBytes  int64
	admittedBytes      int64
}

// frameShapeIdentity identifies the immutable prepared slices. The
// backing pointers distinguish revisions and synthetic loaded functions while
// keeping the idle-pool compatibility check constant time.
type frameShapeIdentity struct {
	code             *ir.MicroCode
	localTypes       *vmType
	localLayouts     *preparedValueLayout
	localVariadic    *bool
	localEscapes     *bool
	temporaryTypes   *vmType
	temporaryLayouts *preparedValueLayout
	temporaryCount   int
	localCount       int
	upvalueCount     int
	resultCount      int
}

func preparedFrameShape(function loadedFunction) frameShapeIdentity {
	shape := frameShapeIdentity{
		code:           function.Decl.Code,
		localCount:     len(function.LocalTypes),
		upvalueCount:   len(function.Decl.Upvalues),
		temporaryCount: len(function.TemporaryTypes),
		resultCount:    len(function.ResultTypes),
	}
	if len(function.LocalTypes) != 0 {
		shape.localTypes = &function.LocalTypes[0]
	}
	if len(function.LocalLayouts) != 0 {
		shape.localLayouts = &function.LocalLayouts[0]
	}
	if len(function.LocalVariadic) != 0 {
		shape.localVariadic = &function.LocalVariadic[0]
	}
	if len(function.LocalEscapes) != 0 {
		shape.localEscapes = &function.LocalEscapes[0]
	}
	if len(function.TemporaryTypes) != 0 {
		shape.temporaryTypes = &function.TemporaryTypes[0]
	}
	if len(function.TemporaryLayouts) != 0 {
		shape.temporaryLayouts = &function.TemporaryLayouts[0]
	}
	return shape
}

type slot struct {
	mu          *sync.Mutex
	typ         vmType
	variadic    bool
	module      *moduleInstance
	value       vmValue
	initialized bool
	managedCell *managedCellStorage
	directOwner *frame
	directIndex int
	inlineOwner *frame
	inlineIndex int
}

const (
	maxIdleFramesPerFunction   = 8
	maxIdleFrameBytesPerModule = 8 << 20
	maxPhysicalFrameCacheBytes = 8 << 20
	maxInlineAggregateBytes    = 128
)

func newSlot(typ vmType, module *moduleInstance, variadic bool) *slot {
	return &slot{mu: new(sync.Mutex), typ: typ, variadic: variadic, module: module}
}

// snapshot does not materialize a zero value, so tracing never creates roots.
func (s *slot) snapshot() (vmValue, bool) {
	if s == nil {
		return vmValue{}, false
	}
	if owner := s.inlineOwner; owner != nil && s.inlineIndex >= 0 && s.inlineIndex < len(owner.inlineInitialized) {
		if !owner.inlineInitialized[s.inlineIndex] {
			return vmValue{}, false
		}
		value := owner.readInlineLocal(s.inlineIndex)
		value.inlineOwner, value.inlineIndex = owner, s.inlineIndex
		return value, true
	}
	if s.managedCell != nil {
		value, err := s.managedCell.read()
		if err != nil {
			return vmValue{}, false
		}
		return value, true
	}
	if owner := s.directOwner; owner != nil && s.directIndex >= 0 && s.directIndex < len(owner.directTypes) {
		value := vmValue{
			Type: owner.directTypes[s.directIndex], Data: vmScalarData,
			scalar: owner.directScalars[s.directIndex], scalarKind: owner.directKinds[s.directIndex],
		}
		if value.Type.Primitive(types.PrimitiveBool) {
			value.Data = value.scalar != 0
		}
		return value, owner.directInitialized[s.directIndex]
	}
	if s.mu == nil {
		return s.value, s.initialized
	}
	s.mu.Lock()
	value, initialized := s.value, s.initialized
	s.mu.Unlock()
	return value, initialized
}

func (s *slot) publish(value vmValue) {
	if owner := s.inlineOwner; owner != nil && s.inlineIndex >= 0 && s.inlineIndex < len(owner.inlineInitialized) {
		if err := owner.writeInlineLocal(s.inlineIndex, value); err != nil {
			owner.recordStorageFailure(fmt.Errorf("publish inline local: %w", err))
		}
		return
	}
	module := s.module
	if module == nil && s.directOwner != nil {
		module = s.directOwner.module
	}
	if s.managedCell != nil {
		if err := s.managedCell.write(value); err != nil {
			module.vm.recordAllocationFailure(fmt.Errorf("publish managed cell: %w", err))
			return
		}
		// The managed page is the cell's canonical storage. Do not retain a
		// second guest value in slot.value after publication.
		s.value = vmValue{}
		s.initialized = false
		return
	}
	if owner := s.directOwner; owner != nil && s.directIndex >= 0 && s.directIndex < len(owner.directTypes) {
		owner.directTypes[s.directIndex] = value.Type
		owner.directScalars[s.directIndex] = value.scalar
		owner.directKinds[s.directIndex] = value.scalarKind
		owner.directInitialized[s.directIndex] = true
		return
	}
	if module != nil {
		value = module.adoptValue(value)
	}
	if s.mu == nil {
		s.value, s.initialized = value, true
		return
	}
	s.mu.Lock()
	s.value, s.initialized = value, true
	s.mu.Unlock()
}

func (s *slot) load() vmValue {
	if s == nil {
		return vmValue{}
	}
	if value, initialized := s.snapshot(); initialized {
		return value
	}
	zero := vmValue{Type: s.typ}
	shape := types.Invalid
	if s.inlineOwner != nil && s.inlineIndex >= 0 && s.inlineIndex < len(s.inlineOwner.inlineLayouts) {
		shape = s.inlineOwner.inlineLayouts[s.inlineIndex].Shape
	}
	if shape == types.Invalid && s.managedCell != nil {
		shape = s.managedCell.layout.Shape
	}
	if shape == types.Invalid {
		shape = s.typ.ShapeKind()
	}
	if s.inlineOwner == nil && s.managedCell == nil || shape != types.Array && shape != types.Struct {
		zero = s.module.zeroValue(s.typ)
	}
	if s.inlineOwner != nil {
		s.publish(zero)
		return s.inlineOwner.readInlineLocal(s.inlineIndex)
	}
	if s.managedCell != nil {
		if err := s.managedCell.write(zero); err != nil {
			s.module.vm.recordAllocationFailure(fmt.Errorf("initialize managed cell: %w", err))
			return vmValue{}
		}
		return zero
	}
	if s.mu == nil {
		s.value, s.initialized = zero, true
		return zero
	}
	s.mu.Lock()
	if !s.initialized {
		s.value, s.initialized = zero, true
	}
	value := s.value
	s.mu.Unlock()
	return value
}

// refreshInlineViewBatch updates the allocation lease carried by a cached
// frame view. A frame can be resumed under a different task batch after a
// cooperative yield; the bytes and view identity remain stable, but writes
// through the view must use the current lease.
func refreshInlineViewBatch(value vmValue, batch *managedAllocationBatch) vmValue {
	switch data := value.Data.(type) {
	case *vmArray:
		if data != nil && data.inlineRaw != nil {
			data.inlineBatch = batch
		}
	case *vmStruct:
		if data != nil && data.inlineRaw != nil {
			data.inlineBatch = batch
		}
	case *vmSlice:
		if data != nil && data.inlineRaw != nil {
			data.inlineBatch = batch
		}
	}
	return value
}

func isInlineView(value vmValue) bool {
	switch data := value.Data.(type) {
	case *vmArray:
		return data != nil && data.inlineRaw != nil
	case *vmStruct:
		return data != nil && data.inlineRaw != nil
	case *vmSlice:
		return data != nil && data.inlineRaw != nil
	default:
		return false
	}
}

func (s *slot) store(value vmValue) error {
	if s == nil {
		return errors.New("nil slot")
	}
	if s.typ.Ref.Kind == types.Any {
		normalized, err := s.module.coerceAssignableRuntimeValue(value, s.typ, false)
		if err != nil {
			return err
		}
		if s.inlineOwner != nil || s.managedCell != nil {
			s.publish(normalized)
		} else {
			s.publish(s.module.cloneValueForStore(normalized))
		}
		return nil
	}
	if s.typ.Equal(value.Type) {
		value.Type = s.typ
		switch s.typ.Ref.Kind {
		case types.Void, types.Primitive, types.Map, types.Pointer, types.Waitable, types.Function:
			s.publish(value)
			return nil
		}
		// Prepared inline and managed destinations already own the value
		// representation. Exact aggregate arguments can be published directly;
		// coercion would only materialize a temporary that the destination
		// immediately encodes again. Escaped plain slots still take the clone
		// path below to preserve ordinary value assignment semantics.
		if s.inlineOwner != nil || s.managedCell != nil {
			s.publish(value)
			return nil
		}
	}
	normalized, err := s.module.coerceAssignableRuntimeValue(value, s.typ, s.variadic)
	if err != nil {
		return err
	}
	if s.inlineOwner != nil || s.managedCell != nil {
		s.publish(normalized)
		return nil
	}
	prepared := s.module.cloneValueForStore(normalized)
	current, _ := s.snapshot()
	s.publish(s.module.assignPreparedValue(current, prepared))
	return nil
}

type frameArgumentSource struct {
	caller   *frame
	operands []ir.Operand
}

func (source *frameArgumentSource) valueAt(index int) (vmValue, error) {
	if source == nil || source.caller == nil || index < 0 || index >= len(source.operands) {
		return vmValue{}, errors.New("invalid direct call argument")
	}
	operand := source.operands[index]
	switch operand.Kind {
	case ir.OperandConstant:
		return source.caller.module.constantValueAt(int(operand.Index))
	case ir.OperandLocal:
		if int(operand.Index) >= len(source.caller.localCells) {
			return vmValue{}, errors.New("direct call local argument out of range")
		}
		return source.caller.localCells[operand.Index].load(), nil
	default:
		return source.caller.slotValue(int(operand.Index)), nil
	}
}

func newFrame(module *moduleInstance, function loadedFunction, args []vmValue, captured []*slot, executionContextID int64, batches ...*managedAllocationBatch) (*frame, bool, error) {
	return newFrameWithArgumentSource(module, function, args, nil, captured, executionContextID, batches...)
}

func newFrameWithArgumentSource(module *moduleInstance, function loadedFunction, args []vmValue, source *frameArgumentSource, captured []*slot, executionContextID int64, batches ...*managedAllocationBatch) (*frame, bool, error) {
	var callFrame *frame
	allocated := true
	module.framePoolMu.Lock()
	pool := module.framePools[function.Decl.ID]
	for last := len(pool) - 1; last >= 0; last-- {
		candidate := pool[last]
		pool[last] = nil
		pool = pool[:last]
		module.framePoolBytes -= candidate.logicalBytes()
		module.framePoolPhysicalBytes -= candidate.idlePhysicalBytes
		if module.vm != nil {
			module.vm.idleFrameBytes.Add(-candidate.idlePhysicalBytes)
		}
		candidate.idlePhysicalBytes = 0
		if frameShapeCompatible(candidate, module, function) {
			callFrame = candidate
			module.framePoolBytes = max(0, module.framePoolBytes)
			module.framePoolPhysicalBytes = max(0, module.framePoolPhysicalBytes)
			allocated = false
			break
		}
		candidate.discardStorage()
	}
	if len(pool) == 0 {
		delete(module.framePools, function.Decl.ID)
	} else {
		module.framePools[function.Decl.ID] = pool
	}
	module.framePoolMu.Unlock()
	if callFrame == nil {
		callFrame = &frame{module: module}
		locals, upvalues, temporaries := len(function.Decl.Locals), len(function.Decl.Upvalues), len(function.Decl.Code.Types)
		popped := 0
		for _, instruction := range function.Instructions {
			if instruction.operands != nil {
				popped = max(popped, len(instruction.operands.Inputs))
			}
		}
		physical := uint64(unsafe.Sizeof(frame{})) +
			uint64(locals)*uint64(unsafe.Sizeof(slot{})+unsafe.Sizeof((*slot)(nil))+unsafe.Sizeof(vmType{})+unsafe.Sizeof(uint64(0))+unsafe.Sizeof(vmScalarKind(0))+2*unsafe.Sizeof(false)+unsafe.Sizeof(uint32(0))+unsafe.Sizeof(preparedValueLayout{})) +
			uint64(locals)*uint64(unsafe.Sizeof((*mapIterator)(nil))) +
			uint64(temporaries)*uint64(unsafe.Sizeof(uint32(0))+unsafe.Sizeof(preparedValueLayout{})+unsafe.Sizeof(vmType{})+2*unsafe.Sizeof(false)+unsafe.Sizeof(vmValue{})) +
			uint64(upvalues)*uint64(unsafe.Sizeof((*slot)(nil))) +
			uint64(popped+len(function.ResultTypes))*uint64(unsafe.Sizeof(vmValue{}))
		if err := callFrame.growResident(physical); err != nil {
			return nil, true, err
		}
		callFrame.localStorage = make([]slot, locals)
		callFrame.localCells = make([]*slot, locals)
		callFrame.directTypes = make([]vmType, locals)
		callFrame.directScalars = make([]uint64, locals)
		callFrame.directKinds = make([]vmScalarKind, locals)
		callFrame.directInitialized = make([]bool, locals)
		callFrame.inlineOffsets = make([]uint32, locals)
		callFrame.inlineLayouts = make([]preparedValueLayout, locals)
		callFrame.inlineInitialized = make([]bool, locals)
		callFrame.tempOffsets = make([]uint32, temporaries)
		callFrame.tempLayouts = make([]preparedValueLayout, temporaries)
		callFrame.tempTypes = make([]vmType, temporaries)
		callFrame.tempInitialized = make([]bool, temporaries)
		callFrame.tempHeap = make([]bool, temporaries)
		for i := range callFrame.localStorage {
			callFrame.localStorage[i] = slot{typ: function.LocalTypes[i], module: module, variadic: function.LocalVariadic[i]}
			callFrame.localCells[i] = &callFrame.localStorage[i]
		}
		callFrame.upvalueCells = make([]*slot, upvalues)
		callFrame.mapIteratorSlots = make([]*mapIterator, locals)
		callFrame.slotValues = make([]vmValue, temporaries)
		callFrame.popValues = make([]vmValue, 0, popped)
		callFrame.returnValues = make([]vmValue, 0, len(function.ResultTypes))
	}
	callFrame.revision = module.revision
	callFrame.module = module
	callFrame.function = function
	callFrame.shape = preparedFrameShape(function)
	if len(batches) != 0 {
		callFrame.allocationBatch = batches[0]
	}
	callFrame.frameLogicalBytes = preparedFrameLogicalBytes(module, function, len(captured))
	callFrame.executionContextID = executionContextID
	callFrame.pc = 0
	callFrame.slotPC, callFrame.slotOperands = -1, nil
	callFrame.slotInput, callFrame.slotOutput = 0, 0
	callFrame.typeDispatchIndex = 0
	callFrame.typeDispatchValue, callFrame.typeDispatchActive = vmValue{}, false
	// A pool belongs to one function and revision. Its prepared offsets and
	// layouts remain valid; recycle already cleared all invocation payload.
	if allocated {
		if err := callFrame.inlineBank(module, function); err != nil {
			callFrame.discardStorage()
			return nil, allocated, err
		}
		if err := callFrame.tempBank(module, function); err != nil {
			callFrame.discardStorage()
			return nil, allocated, err
		}
	}
	callFrame.defers = callFrame.defers[:0]
	argumentCount := len(args)
	if source != nil {
		argumentCount = len(source.operands)
	}
	for i, local := range function.Decl.Locals {
		cell := &callFrame.localStorage[i]
		// A pooled activation has the same shape but must rebuild ownership
		// links for this invocation. Keeping a stale direct/inline owner here
		// makes a slot read through the previous activation's bank and also
		// prevents an escaped local from taking the managed-cell path.
		cell.directOwner, cell.inlineOwner = nil, nil
		cell.directIndex, cell.inlineIndex = 0, 0
		cell.managedCell = nil
		cell.value, cell.initialized = vmValue{}, false
		if i < len(function.LocalEscapes) && function.LocalEscapes[i] {
			cell = newSlot(function.LocalTypes[i], module, function.LocalVariadic[i])
		} else {
			if directFrameLocal(cell.typ) {
				cell.directOwner, cell.directIndex = callFrame, i
			} else if callFrame.inlineLayouts[i].Stride != 0 {
				cell.inlineOwner, cell.inlineIndex = callFrame, i
			}
			if cell.directOwner == nil && cell.inlineOwner == nil && module != nil && module.vm != nil {
				if _, err := cell.ensureManagedCell(); err != nil {
					callFrame.recycle()
					return nil, allocated, fmt.Errorf("local %s storage: %w", local.ID, err)
				}
			}
		}
		callFrame.localCells[i] = cell
		if i < argumentCount {
			var value vmValue
			if source != nil {
				var err error
				value, err = source.valueAt(i)
				if err != nil {
					callFrame.recycle()
					return nil, allocated, fmt.Errorf("argument %s: %w", local.ID, err)
				}
			} else {
				value = args[i]
			}
			if err := cell.store(value); err != nil {
				callFrame.recycle()
				return nil, allocated, fmt.Errorf("argument %s: %w", local.ID, err)
			}
		}
	}
	for i, upvalue := range function.Decl.Upvalues {
		capturedSlot := (*slot)(nil)
		if i < len(captured) {
			capturedSlot = captured[i]
		}
		if capturedSlot == nil {
			callFrame.recycle()
			return nil, allocated, fmt.Errorf("missing upvalue %q", upvalue.ID)
		}
		callFrame.upvalueCells[i] = capturedSlot
	}
	return callFrame, allocated, nil
}

func frameShapeCompatible(candidate *frame, module *moduleInstance, function loadedFunction) bool {
	return candidate != nil && candidate.module == module && candidate.revision == module.revision && candidate.shape == preparedFrameShape(function)
}

func (f *frame) recycle() {
	if f == nil || f.module == nil {
		return
	}
	module := f.module
	functionID := f.function.Decl.ID
	if f.execution.pinnedRevision != nil {
		f.execution.pinnedRevision.release()
	}
	if f.allocationAdmitted && f.admittedBytes > 0 && module.vm != nil {
		module.vm.releaseAllocationBytes(f.admittedBytes)
	}
	f.allocationAdmitted = false
	f.admittedBytes = 0
	f.execution = executionFrame{}
	for i := range f.localCells {
		f.localStorage[i].value = vmValue{}
		f.localStorage[i].initialized = false
		f.localStorage[i].managedCell = nil
		f.localStorage[i].directOwner = nil
		f.localStorage[i].inlineOwner = nil
		f.localStorage[i].directIndex = 0
		f.localStorage[i].inlineIndex = 0
		// Bank locations belong to this function's reusable frame shape.
		// Only activation data and escaped identities are released here.
		f.localCells[i] = &f.localStorage[i]
	}
	clear(f.directTypes)
	clear(f.directScalars)
	clear(f.directKinds)
	clear(f.directInitialized)
	clear(f.inlineStorage)
	clear(f.inlineInitialized)
	clear(f.tempStorage)
	clear(f.tempInitialized)
	for i := range f.upvalueCells {
		f.upvalueCells[i] = nil
	}
	// Every operand/defer truncation clears the released range. Only the live
	// high-water marks remain to clear here, irrespective of retained capacity.
	clear(f.slotValues)
	f.slotOperands = nil
	clear(f.popValues)
	clear(f.returnValues)
	for index := range f.defers {
		module.vm.releaseAllocationBytes(f.defers[index].admittedBytes)
	}
	clear(f.defers)
	for _, iterator := range f.mapIteratorSlots {
		if iterator != nil {
			iterator.release(module.vm)
		}
	}
	clear(f.mapIteratorSlots)
	f.typeDispatchValue, f.typeDispatchActive = vmValue{}, false
	f.popValues = f.popValues[:0]
	f.returnValues = f.returnValues[:0]
	f.defers = f.defers[:0]
	module.framePoolMu.Lock()
	defer module.framePoolMu.Unlock()
	if module.framePools == nil {
		module.framePools = make(map[string][]*frame)
	}
	pool := module.framePools[functionID]
	logicalBytes := f.logicalBytes()
	if len(pool) < maxIdleFramesPerFunction && module.framePoolBytes <= maxIdleFrameBytesPerModule-logicalBytes {
		physicalBytes := int64(f.residentBytes)
		retainPhysical := physicalBytes <= maxPhysicalFrameCacheBytes-module.framePoolPhysicalBytes
		if module.vm != nil {
			retainPhysical = false
			if module.revision == module.vm.revision.Load() {
				for {
					retained := module.vm.idleFrameBytes.Load()
					if physicalBytes > maxPhysicalFrameCacheBytes-retained {
						break
					}
					if module.vm.idleFrameBytes.CompareAndSwap(retained, retained+physicalBytes) {
						retainPhysical = true
						break
					}
				}
			}
		}
		if retainPhysical {
			f.idlePhysicalBytes = physicalBytes
			module.framePoolPhysicalBytes += physicalBytes
			module.framePools[functionID] = append(pool, f)
			module.framePoolBytes += logicalBytes
			return
		}
	}
	f.discardStorage()
}

// growResident admits actual frame backing before the corresponding host
// allocation. Cached frames remain charged until their buffers are discarded.
func (f *frame) growResident(bytes uint64) error {
	if f.module != nil && f.module.vm != nil && f.module.vm.heap != nil {
		heap := f.module.vm.heap
		admission := bytes
		if f.residentHeap == nil {
			admission += f.residentBytes
		}
		heap.mu.Lock()
		admitted := !heap.closed && heap.admitResident(admission)
		heap.mu.Unlock()
		if !admitted {
			return errManagedLimit
		}
		f.residentHeap = heap
	}
	f.residentBytes += bytes
	return nil
}

func (f *frame) discardStorage() {
	if heap := f.residentHeap; heap != nil {
		heap.mu.Lock()
		if !heap.closed {
			heap.counters.residentPageBytes -= f.residentBytes
		}
		heap.mu.Unlock()
	}
	*f = frame{}
}

func directFrameLocal(typ vmType) bool {
	if typ.Primitive(types.PrimitiveBool) {
		return true
	}
	info, numeric := typ.NumericInfo()
	return numeric && info.Kind != types.NumericComplex
}

// inlineBank prepares small aggregate locals once per frame shape. Keeping
// these values in one contiguous frame allocation avoids a managed heap
// record for every activation while preserving the typed byte layout used by
// the heap codec, including its reference lanes.
func (f *frame) inlineBank(module *moduleInstance, function loadedFunction) error {
	if f == nil || module == nil || module.vm == nil || module.vm.valueLayouts == nil {
		return nil
	}
	clear(f.inlineOffsets)
	clear(f.inlineLayouts)
	var total uint32
	for index, typ := range function.LocalTypes {
		if index >= len(f.inlineLayouts) || (index < len(function.LocalEscapes) && function.LocalEscapes[index]) || directFrameLocal(typ) {
			continue
		}
		layout := preparedValueLayout{}
		if index < len(function.LocalLayouts) {
			layout = function.LocalLayouts[index]
		}
		if layout.Stride == 0 {
			var err error
			layout, err = module.vm.valueLayouts.layoutFor(typ)
			if err != nil {
				continue
			}
		}
		if !inlineFrameLayout(layout) {
			continue
		}
		if remainder := total % 8; remainder != 0 {
			total += 8 - remainder
		}
		f.inlineOffsets[index] = total
		f.inlineLayouts[index] = layout
		total += layout.Stride
	}
	if total == 0 {
		f.inlineStorage = f.inlineStorage[:0]
		return nil
	}
	if cap(f.inlineStorage) >= int(total) {
		f.inlineStorage = f.inlineStorage[:total]
		clear(f.inlineStorage)
	} else {
		if err := f.growResident(uint64(total) - uint64(cap(f.inlineStorage))); err != nil {
			return err
		}
		f.inlineStorage = make([]byte, total)
	}
	return nil
}

func (f *frame) tempBank(module *moduleInstance, function loadedFunction) error {
	if f == nil || module == nil || module.vm == nil || module.vm.valueLayouts == nil {
		return nil
	}
	clear(f.tempOffsets)
	clear(f.tempLayouts)
	clear(f.tempTypes)
	clear(f.tempHeap)
	var total uint32
	for index, ref := range function.Decl.Code.Types {
		typ := module.runtimeType(ref)
		layout := preparedValueLayout{}
		if index < len(function.TemporaryTypes) {
			typ = function.TemporaryTypes[index]
		}
		if index < len(function.TemporaryLayouts) {
			layout = function.TemporaryLayouts[index]
		}
		if layout.Stride == 0 {
			var err error
			layout, err = module.vm.valueLayouts.layoutFor(typ)
			if err != nil {
				continue
			}
		}
		if layout.Stride == 0 {
			continue
		}
		if !inlineFrameLayout(layout) {
			_, numeric := typ.NumericInfo()
			if !(typ.Primitive(types.PrimitiveBool) || numeric || typ.ShapeKind() == types.Array || typ.ShapeKind() == types.Struct) {
				continue
			}
			if typ.ShapeKind() == types.Array || typ.ShapeKind() == types.Struct {
				f.tempLayouts[index], f.tempTypes[index], f.tempHeap[index] = layout, typ, true
				continue
			}
		}
		if remainder := total % 8; remainder != 0 {
			total += 8 - remainder
		}
		f.tempOffsets[index] = total
		f.tempLayouts[index] = layout
		f.tempTypes[index] = typ
		total += layout.Stride
	}
	if cap(f.tempStorage) >= int(total) {
		f.tempStorage = f.tempStorage[:total]
		clear(f.tempStorage)
	} else if total == 0 {
		f.tempStorage = f.tempStorage[:0]
	} else {
		if err := f.growResident(uint64(total) - uint64(cap(f.tempStorage))); err != nil {
			return err
		}
		f.tempStorage = make([]byte, total)
	}
	return nil
}

func inlineFrameLayout(layout preparedValueLayout) bool {
	return layout.Stride != 0 && layout.Logical <= maxInlineAggregateBytes
}

func (f *frame) inlineLayout(index int) (preparedValueLayout, []byte, bool) {
	if f == nil || index < 0 || index >= len(f.inlineLayouts) || f.inlineLayouts[index].Stride == 0 {
		return preparedValueLayout{}, nil, false
	}
	offset := f.inlineOffsets[index]
	end := uint64(offset) + uint64(f.inlineLayouts[index].Stride)
	if end > uint64(len(f.inlineStorage)) {
		return preparedValueLayout{}, nil, false
	}
	return f.inlineLayouts[index], f.inlineStorage[offset:end], true
}

func (f *frame) readInlineLocal(index int) vmValue {
	layout, raw, ok := f.inlineLayout(index)
	if !ok {
		return vmValue{Type: f.localStorage[index].typ}
	}
	if cached := f.localStorage[index].value; isInlineView(cached) {
		cached = refreshInlineViewBatch(cached, f.allocationBatch)
		cached.inlineOwner, cached.inlineIndex, cached.inlineTemp = f, index, false
		f.localStorage[index].value = cached
		return cached
	}
	value := readInlineAggregateOwned(f.module, f.localStorage[index].typ, layout, raw, f.allocationBatch)
	value.inlineOwner, value.inlineIndex = f, index
	if isInlineView(value) {
		f.localStorage[index].value = value
	}
	return value
}

func (f *frame) tempLayout(index int) (preparedValueLayout, []byte, bool) {
	if f == nil || index < 0 || index >= len(f.tempLayouts) || f.tempLayouts[index].Stride == 0 {
		return preparedValueLayout{}, nil, false
	}
	if f.tempHeap[index] {
		return preparedValueLayout{}, nil, false
	}
	offset := f.tempOffsets[index]
	end := uint64(offset) + uint64(f.tempLayouts[index].Stride)
	if end > uint64(len(f.tempStorage)) {
		return preparedValueLayout{}, nil, false
	}
	return f.tempLayouts[index], f.tempStorage[offset:end], true
}

func (f *frame) readTempValue(index int) vmValue {
	if index >= 0 && index < len(f.tempHeap) && f.tempHeap[index] {
		if index >= len(f.tempInitialized) || !f.tempInitialized[index] {
			return vmValue{Type: f.tempTypes[index]}
		}
		return f.slotValues[index]
	}
	if index >= 0 && index < len(f.slotValues) && f.slotValues[index].inlineOwner != nil {
		return refreshInlineViewBatch(f.slotValues[index], f.allocationBatch)
	}
	layout, raw, ok := f.tempLayout(index)
	if !ok {
		return f.slotValues[index]
	}
	value := readInlineAggregateOwned(f.module, f.tempTypes[index], layout, raw, f.allocationBatch)
	value.inlineOwner, value.inlineIndex, value.inlineTemp = f, index, true
	if isInlineView(value) {
		f.slotValues[index] = value
	}
	return value
}

func (f *frame) writeTempValue(index int, value vmValue) error {
	if index >= 0 && index < len(f.tempHeap) && f.tempHeap[index] {
		published := f.module.adoptValueWithBatch(value, f.allocationBatch)
		f.slotValues[index] = published
		f.tempInitialized[index] = true
		return nil
	}
	_, raw, ok := f.tempLayout(index)
	if !ok {
		return fmt.Errorf("temporary %d is not prepared", index)
	}
	if value.scalarKind != 0 || value.Type.Primitive(types.PrimitiveBool) {
		if err := writeScalarBytes(raw, 0, value); err != nil {
			return err
		}
		f.slotValues[index] = vmValue{}
		f.tempInitialized[index] = true
		return nil
	}
	if f.copyInlineValue(raw, f.tempLayouts[index], value) {
		f.slotValues[index] = vmValue{}
		f.tempInitialized[index] = true
		return nil
	}
	if value.inlineOwner == nil || value.inlineOwner != f {
		if err := f.module.writeInlineValueWithLayout(raw, 0, value, f.tempLayouts[index], map[uintptr]guestRef{}, f.allocationBatch); err != nil {
			return err
		}
		f.slotValues[index] = vmValue{}
		f.tempInitialized[index] = true
		return nil
	}
	var scratch [maxInlineAggregateBytes]byte
	prepared := scratch[:min(len(scratch), len(raw))]
	if len(raw) > len(scratch) {
		prepared = make([]byte, len(raw))
	}
	if err := f.module.writeInlineValueWithLayout(prepared, 0, value, f.tempLayouts[index], map[uintptr]guestRef{}, f.allocationBatch); err != nil {
		return err
	}
	copy(raw, prepared)
	f.slotValues[index] = vmValue{}
	f.tempInitialized[index] = true
	return nil
}

func (f *frame) clearSlotValue(index int) {
	if index < 0 {
		return
	}
	if index < len(f.tempInitialized) && f.tempLayouts[index].Stride != 0 {
		f.tempInitialized[index] = false
		if index < len(f.slotValues) {
			f.slotValues[index] = vmValue{}
		}
		if index < len(f.tempHeap) && f.tempHeap[index] {
			return
		}
		offset := f.tempOffsets[index]
		end := offset + f.tempLayouts[index].Stride
		if int(end) <= len(f.tempStorage) {
			clear(f.tempStorage[offset:end])
		}
		return
	}
	if index < len(f.slotValues) {
		f.slotValues[index] = vmValue{}
	}
}

func (f *frame) slotValue(index int) vmValue {
	if index >= 0 && index < len(f.tempInitialized) && f.tempLayouts[index].Stride != 0 {
		if f.tempHeap[index] {
			return f.slotValues[index]
		}
		if index < len(f.slotValues) && f.slotValues[index].inlineOwner != nil {
			return refreshInlineViewBatch(f.slotValues[index], f.allocationBatch)
		}
		if !f.tempInitialized[index] {
			return vmValue{Type: f.tempTypes[index]}
		}
		return f.readTempValue(index)
	}
	return f.slotValues[index]
}

func (f *frame) storeSlotValue(index int, value vmValue) {
	if index >= 0 && index < len(f.tempLayouts) && f.tempLayouts[index].Stride != 0 {
		if index < len(f.tempHeap) && f.tempHeap[index] {
			if err := f.writeTempValue(index, value); err != nil {
				f.recordStorageFailure(fmt.Errorf("store temporary %d: %w", index, err))
			}
			return
		}
		// A local aggregate loaded into an operand remains a borrow of the
		// addressable local for field/index mutation. Temporary-to-temporary
		// transfers, below, use a byte copy so ordinary value assignment does
		// not create an alias.
		if value.inlineOwner == f && !value.inlineTemp {
			f.slotValues[index] = value
			f.tempInitialized[index] = false
			return
		}
		if err := f.writeTempValue(index, value); err != nil {
			f.recordStorageFailure(fmt.Errorf("store temporary %d: %w", index, err))
		}
		return
	}
	if value.inlineOwner == nil {
		value = f.module.adoptValueWithBatch(value, f.allocationBatch)
	}
	f.slotValues[index] = value
}

func (f *frame) writeInlineLocal(index int, value vmValue) error {
	layout, raw, ok := f.inlineLayout(index)
	if !ok {
		return fmt.Errorf("inline local %d is not prepared", index)
	}
	if f.copyInlineValue(raw, layout, value) {
		f.inlineInitialized[index] = true
		return nil
	}
	if value.inlineOwner == nil || value.inlineOwner != f {
		if err := f.module.writeInlineValueWithLayout(raw, 0, value, layout, map[uintptr]guestRef{}, f.allocationBatch); err != nil {
			return err
		}
		f.inlineInitialized[index] = true
		return nil
	}
	var scratch [maxInlineAggregateBytes]byte
	prepared := scratch[:min(len(scratch), len(raw))]
	if len(raw) > len(scratch) {
		prepared = make([]byte, len(raw))
	}
	if err := f.module.writeInlineValueWithLayout(prepared, 0, value, f.inlineLayouts[index], map[uintptr]guestRef{}, f.allocationBatch); err != nil {
		return err
	}
	copy(raw, prepared)
	f.inlineInitialized[index] = true
	return nil
}

// copyInlineValue copies a prepared aggregate directly between frame banks.
// The source may belong to this activation or to a callee that is still
// completing. The scheduler recycles that source only after the destination
// has published the result; values retained past that point are converted to
// an owned managed value by the result boundary.
func (f *frame) copyInlineValue(dst []byte, layout preparedValueLayout, value vmValue) bool {
	if f == nil || value.inlineOwner == nil {
		return false
	}
	owner := value.inlineOwner
	var sourceLayout preparedValueLayout
	var source []byte
	var ok bool
	if value.inlineTemp {
		sourceLayout, source, ok = owner.tempLayout(value.inlineIndex)
	} else {
		sourceLayout, source, ok = owner.inlineLayout(value.inlineIndex)
	}
	if !ok || sourceLayout.ID != layout.ID || sourceLayout.Stride != layout.Stride || len(source) != len(dst) {
		return false
	}
	// A field or element view may retain the same frame owner/index while its
	// inline bytes begin at an offset inside the parent aggregate. Only the
	// exact frame-bank view can be copied as a whole; otherwise the operation
	// would silently replace a destination with the parent's first bytes.
	if view, ok := inlineValueBytes(value); ok {
		if len(view) != len(source) || len(view) == 0 || unsafe.Pointer(&view[0]) != unsafe.Pointer(&source[0]) {
			return false
		}
	}
	copy(dst, source)
	return true
}

func inlineValueBytes(value vmValue) ([]byte, bool) {
	switch data := value.Data.(type) {
	case *vmArray:
		if data != nil && data.inlineRaw != nil {
			return data.inlineRaw, true
		}
	case *vmStruct:
		if data != nil && data.inlineRaw != nil {
			return data.inlineRaw, true
		}
	case *vmSlice:
		if data != nil && data.inlineRaw != nil {
			return data.inlineRaw, true
		}
	}
	return nil, false
}

func (f *frame) recordStorageFailure(err error) {
	if f.allocationBatch != nil {
		f.allocationBatch.failure = err
	} else {
		f.module.vm.recordAllocationFailure(err)
	}
}

func readInlineAggregateOwned(module *moduleInstance, typ vmType, layout preparedValueLayout, src []byte, batch *managedAllocationBatch) vmValue {
	shape := layout.Shape
	primitive := layout.Primitive
	numericKind, numericBits := layout.NumericKind, layout.NumericBits
	if shape == types.Invalid {
		// Values crossing an API boundary may not carry a prepared descriptor.
		// Keep that slow fallback while all frame-owned layouts use the cached
		// classification above.
		shape = typ.ShapeKind()
		if underlying := typ.Underlying(); underlying.Ref.Kind == types.Primitive {
			primitive = underlying.Ref.Primitive
		}
		if info, ok := typ.NumericInfo(); ok {
			numericKind, numericBits = info.Kind, uint16(info.Bits)
		}
	}
	if numericKind != types.NumericInvalid {
		return decodeScalarBytesWithInfo(typ, src, types.NumericInfo{
			Primitive: primitive,
			Kind:      numericKind,
			Bits:      int(numericBits),
		}, true)
	}
	if primitive == types.PrimitiveBool {
		return newVMValue(typ, len(src) != 0 && src[0] != 0)
	}
	switch shape {
	case types.Array:
		length, element, ok := typ.ArrayInfo()
		if !ok {
			return vmValue{Type: typ}
		}
		var elementLayout preparedValueLayout
		if layout.ArrayElement != nil {
			elementLayout = *layout.ArrayElement
		} else {
			var err error
			elementLayout, err = module.vm.valueLayouts.layoutFor(element)
			if err != nil {
				return vmValue{Type: typ}
			}
		}
		stride := elementLayout.Stride
		if stride == 0 && length != 0 {
			stride = ir.StorageScalarBytes
		}
		if length*uint64(stride) > uint64(len(src)) {
			return vmValue{Type: typ}
		}
		return vmValue{Type: typ, Data: &vmArray{vmSlice: vmSlice{
			Start: 0, Len: int(length), Cap: int(length), inlineRaw: src,
			inlineType: typ, inlineElemType: element, inlineElem: elementLayout,
			inlineModule: module, inlineBatch: batch,
		}}}
	case types.Struct:
		plan := layout.StructPlan
		if plan == nil {
			var err error
			plan, err = module.vm.valueLayouts.structLayout(typ)
			if err != nil {
				return vmValue{Type: typ}
			}
		}
		if plan == nil {
			return vmValue{Type: typ}
		}
		return vmValue{Type: typ, Data: &vmStruct{
			schema: plan.schema, managedFields: plan.fields, inlineRaw: src, inlineType: typ, inlineLayout: layout,
			inlineModule: module, inlineBatch: batch,
		}}
	case types.Any, types.Interface:
		if module == nil || module.vm == nil {
			return vmValue{Type: typ}
		}
		encoded, err := decodeManagedDynamicWithRegistry(src, typ, module.vm.heap, module.vm.valueLayouts)
		if err != nil || encoded.representation == managedValueEmpty {
			return vmValue{Type: typ}
		}
		decoded := decodeManagedValueWithLayouts(encoded, module.vm.heap, module.vm.valueLayouts, module.vm)
		return vmValue{Type: typ, Data: decoded, module: module, heap: module.vm.heap, owner: module.vm, layouts: module.vm.valueLayouts}
	case types.Primitive:
		if primitive != types.PrimitiveString {
			break
		}
		if len(src) < 24 || module == nil || module.vm == nil {
			return vmValue{Type: typ}
		}
		backing := guestRef{slot: binary.LittleEndian.Uint32(src), generation: binary.LittleEndian.Uint32(src[4:])}
		start := binary.LittleEndian.Uint64(src[8:])
		length := binary.LittleEndian.Uint64(src[16:])
		if !backing.valid() {
			return newVMValue(typ, "")
		}
		if start > uint64(^uint32(0)) || length > uint64(^uint(0)>>1) {
			return vmValue{Type: typ}
		}
		text, err := module.vm.heap.readString(backing, start, length)
		if err != nil {
			return vmValue{Type: typ}
		}
		return newVMValue(typ, text)
	case types.Slice:
		if len(src) < 32 || module == nil || module.vm == nil {
			return vmValue{Type: typ}
		}
		backing := guestRef{slot: binary.LittleEndian.Uint32(src), generation: binary.LittleEndian.Uint32(src[4:])}
		start, length, capacity := binary.LittleEndian.Uint64(src[8:]), binary.LittleEndian.Uint64(src[16:]), binary.LittleEndian.Uint64(src[24:])
		if !backing.valid() {
			return newVMValue(typ, (*vmSlice)(nil))
		}
		if start > uint64(^uint32(0)) || length > uint64(^uint(0)>>1) || capacity > uint64(^uint(0)>>1) {
			return vmValue{Type: typ}
		}
		element, ok := typ.SliceElem()
		if !ok {
			return vmValue{Type: typ}
		}
		var elementLayout preparedValueLayout
		if layout.SliceElement != nil {
			elementLayout = *layout.SliceElement
		} else {
			var err error
			elementLayout, err = module.vm.valueLayouts.layoutFor(element)
			if err != nil {
				return vmValue{Type: typ}
			}
		}
		return newVMValue(typ, &vmSlice{managedHeap: module.vm.heap, managedRef: backing, managedBase: uint32(start), Len: int(length), Cap: int(capacity), managedType: element, managedElem: elementLayout, managedByte: element.Primitive(types.PrimitiveUint8), managedLayouts: module.vm.valueLayouts, managedOwner: module.vm})
	case types.Pointer:
		if len(src) < ir.StoragePointerBytes || module == nil || module.vm == nil {
			return vmValue{Type: typ}
		}
		target := guestRef{slot: binary.LittleEndian.Uint32(src), generation: binary.LittleEndian.Uint32(src[4:])}
		location := binary.LittleEndian.Uint64(src[8:])
		if !target.valid() || location > uint64(^uint32(0)) {
			return vmValue{Type: typ}
		}
		return vmValue{Type: typ, ref: target, offset: uint32(location), heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}
	}
	if shape == types.Map || shape == types.Waitable || shape == types.Function || primitive == types.PrimitiveFunction {
		if len(src) < 8 || module == nil || module.vm == nil {
			return vmValue{Type: typ}
		}
		ref := guestRef{slot: binary.LittleEndian.Uint32(src), generation: binary.LittleEndian.Uint32(src[4:])}
		if !ref.valid() {
			return vmValue{Type: typ}
		}
		return vmValue{Type: typ, ref: ref, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}
	}
	if len(layout.References) != 0 && len(src) >= 8 {
		child := guestRef{slot: binary.LittleEndian.Uint32(src), generation: binary.LittleEndian.Uint32(src[4:])}
		return vmValue{Type: typ, ref: child, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}
	}
	return decodeScalarBytes(typ, src)
}

func (f *frame) referenceLive(index int) bool {
	if f == nil {
		return false
	}
	if len(f.function.ReferenceMaps) == 0 {
		return true
	}
	pc := f.pc
	if pc < 0 {
		pc = 0
	}
	if pc >= len(f.function.ReferenceMaps) {
		pc = len(f.function.ReferenceMaps) - 1
	}
	bitmap := f.function.ReferenceMaps[pc]
	word := index / 64
	return word >= 0 && word < len(bitmap) && bitmap[word]&(uint64(1)<<uint(index%64)) != 0
}

func (f *frame) logicalBytes() int64 {
	if f == nil {
		return 0
	}
	if f.frameLogicalBytes != 0 {
		return f.frameLogicalBytes
	}
	return ir.StorageFrameHeaderBytes + int64(cap(f.localStorage)+cap(f.upvalueCells)+cap(f.slotValues)+cap(f.popValues)+cap(f.returnValues)+cap(f.defers))*ir.StorageReferenceBytes
}

func preparedFrameLogicalBytes(module *moduleInstance, function loadedFunction, upvalues int) int64 {
	if function.FrameValueBytes != 0 {
		return function.FrameValueBytes + int64(upvalues)*ir.StorageReferenceBytes
	}
	total := int64(ir.StorageFrameHeaderBytes)
	addLayout := func(layout preparedValueLayout) {
		if layout.Stride == 0 || layout.Logical > maxInlineAggregateBytes {
			total += ir.StorageReferenceBytes
			return
		}
		total += int64(layout.Logical)
	}
	addType := func(typ vmType) {
		if module == nil || module.vm == nil || module.vm.valueLayouts == nil {
			total += ir.StorageReferenceBytes
			return
		}
		layout, err := module.vm.valueLayouts.layoutFor(typ)
		if err != nil {
			total += ir.StorageReferenceBytes
			return
		}
		addLayout(layout)
	}
	for index, typ := range function.LocalTypes {
		if index < len(function.LocalEscapes) && function.LocalEscapes[index] {
			total += ir.StorageReferenceBytes
		} else if index < len(function.LocalLayouts) {
			addLayout(function.LocalLayouts[index])
		} else {
			addType(typ)
		}
	}
	for index, ref := range function.Decl.Code.Types {
		if index < len(function.TemporaryLayouts) {
			addLayout(function.TemporaryLayouts[index])
		} else if module != nil {
			addType(module.runtimeType(ref))
		} else {
			total += ir.StorageReferenceBytes
		}
	}
	total += int64(upvalues) * ir.StorageReferenceBytes
	for index := range function.ResultTypes {
		if index < len(function.ResultLayouts) {
			addLayout(function.ResultLayouts[index])
		} else {
			addType(function.ResultTypes[index])
		}
	}
	return total
}

func (f *frame) normalizeReturnValues(values []vmValue) ([]vmValue, error) {
	values, err := f.validateReturnValues(values)
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		values[i] = f.module.cloneValueForStore(value)
	}
	return values, nil
}

// validateReturnValues performs the type boundary without detaching a frame
// owned aggregate. The scheduler publishes these values to the caller before
// recycling this frame; only an external result or continuation needs the
// owning clone performed by normalizeReturnValues.
func (f *frame) validateReturnValues(values []vmValue) ([]vmValue, error) {
	if f == nil {
		return nil, errors.New("nil frame")
	}
	if len(values) != len(f.function.ResultTypes) {
		return nil, fmt.Errorf("return value count mismatch: got %d, want %d", len(values), len(f.function.ResultTypes))
	}
	for i, value := range values {
		normalized, err := f.module.coerceAssignableRuntimeValue(value, f.function.ResultTypes[i], false)
		if err != nil {
			return nil, fmt.Errorf("return value %d: %w", i, err)
		}
		values[i] = normalized
	}
	return values, nil
}

func (f *frame) hasResultLocals() bool {
	return f != nil && len(f.function.Decl.ResultLocals) != 0
}

func (f *frame) currentResultValues() ([]vmValue, error) {
	if f == nil {
		return nil, errors.New("nil frame")
	}
	if len(f.function.ResultTypes) == 0 {
		return nil, nil
	}
	if len(f.function.Decl.ResultLocals) == 0 {
		clear(f.returnValues)
		values := f.returnValues[:0]
		for _, typ := range f.function.ResultTypes {
			values = append(values, f.module.zeroValue(typ))
		}
		f.returnValues = values
		return f.validateReturnValues(values)
	}
	if len(f.function.Decl.ResultLocals) != len(f.function.ResultTypes) {
		return nil, fmt.Errorf("result local count mismatch: got %d, want %d", len(f.function.Decl.ResultLocals), len(f.function.ResultTypes))
	}
	clear(f.returnValues)
	values := f.returnValues[:0]
	for _, local := range f.function.Decl.ResultLocals {
		index, ok := f.function.LocalIndexes[local]
		if !ok || index < 0 || index >= len(f.localCells) || f.localCells[index] == nil {
			return nil, fmt.Errorf("unknown result local %q", local)
		}
		values = append(values, f.localCells[index].load())
	}
	f.returnValues = values
	return f.validateReturnValues(values)
}

func (f *frame) push(value vmValue) {
	// The destination owns publication. Typed banks can encode the value
	// directly without allocating an intermediate heap descriptor.
	target := f.slotOperands.Outputs[f.slotOutput]
	if target&ir.LocalOutput != 0 {
		f.localCells[target&^ir.LocalOutput].publish(value)
	} else {
		f.storeSlotValue(int(target), value)
	}
	f.slotOutput++
}

// copyLocalToOutput forwards a prepared local without decoding its bytes into
// a general vmValue first. It handles the three storage pairs that can prove
// identical at load time: scalar lanes, inline frame layouts, and managed
// cells with the same heap layout. Returning false keeps dynamic and escaped
// values on the ordinary slot path.
func (f *frame) copyLocalToOutput(source int) (bool, error) {
	if f == nil || source < 0 || source >= len(f.localCells) || f.slotOperands == nil || f.slotOutput >= len(f.slotOperands.Outputs) {
		return false, nil
	}
	target := f.slotOperands.Outputs[f.slotOutput]
	// Outputs used by mutation must keep the local view so the mutation reaches
	// the addressable source. Direct forwarding is only safe between locals.
	if target&ir.LocalOutput == 0 {
		return false, nil
	}
	src := f.localCells[source]
	if src == nil {
		return false, nil
	}
	index := int(target &^ ir.LocalOutput)
	if index < 0 || index >= len(f.localCells) {
		return false, nil
	}
	dst := f.localCells[index]
	if dst == nil {
		return false, nil
	}
	if src.directOwner == f && dst.directOwner == f && src.directIndex >= 0 && dst.directIndex >= 0 && src.directIndex < len(f.directInitialized) && dst.directIndex < len(f.directInitialized) {
		if !f.directInitialized[src.directIndex] {
			return false, nil
		}
		f.directTypes[dst.directIndex] = f.directTypes[src.directIndex]
		f.directScalars[dst.directIndex] = f.directScalars[src.directIndex]
		f.directKinds[dst.directIndex] = f.directKinds[src.directIndex]
		f.directInitialized[dst.directIndex] = true
		f.slotOutput++
		return true, nil
	}
	if src.inlineOwner == f && dst.inlineOwner == f && src.inlineIndex >= 0 && dst.inlineIndex >= 0 && src.inlineIndex < len(f.inlineInitialized) {
		destinationLayout, destinationRaw, destinationOK := f.inlineLayout(dst.inlineIndex)
		if destinationOK && f.inlineInitialized[src.inlineIndex] && f.copyInlineValue(destinationRaw, destinationLayout, src.load()) {
			f.inlineInitialized[dst.inlineIndex] = true
			f.slotOutput++
			return true, nil
		}
	}
	if src.managedCell != nil && dst.managedCell != nil && src.managedCell.heap == dst.managedCell.heap && src.managedCell.layout.ID == dst.managedCell.layout.ID {
		if err := src.managedCell.heap.copy(dst.managedCell.ref, 0, src.managedCell.ref, 0, src.managedCell.layout.Stride); err != nil {
			return true, err
		}
		f.slotOutput++
		return true, nil
	}
	return false, nil
}

// pushCopied publishes a value whose source is owned by another storage
// location. It is used by map iteration and direct result producers: the
// destination performs the language-mandated copy once instead of the
// producer first materializing a detached value and the destination copying
// it again. Scalar and reference values retain their existing alias rules.
func (f *frame) pushCopied(value vmValue) error {
	target := f.slotOperands.Outputs[f.slotOutput]
	if target&ir.LocalOutput != 0 {
		if err := f.localCells[target&^ir.LocalOutput].store(value); err != nil {
			return err
		}
	} else {
		f.storeSlotValue(int(target), value)
	}
	f.slotOutput++
	return nil
}

func (f *frame) pop() (vmValue, error) {
	if f.slotInput == 0 {
		return vmValue{}, errors.New("operand underflow")
	}
	f.slotInput--
	operand := f.slotOperands.Inputs[f.slotInput]
	if operand.Kind == ir.OperandConstant {
		return f.module.constantValueAt(int(operand.Index))
	}
	if operand.Kind == ir.OperandLocal {
		return f.localCells[operand.Index].load(), nil
	}
	return f.slotValue(int(operand.Index)), nil
}

func (f *frame) popN(count int) ([]vmValue, error) {
	if count < 0 {
		return nil, fmt.Errorf("negative pop count %d", count)
	}
	if count > f.slotInput {
		return nil, fmt.Errorf("operand underflow: need %d values, have %d", count, f.slotInput)
	}
	start := f.slotInput - count
	clear(f.popValues)
	if cap(f.popValues) < count {
		if err := f.growResident(uint64(count-cap(f.popValues)) * uint64(unsafe.Sizeof(vmValue{}))); err != nil {
			return nil, err
		}
		f.popValues = make([]vmValue, count)
	} else {
		f.popValues = f.popValues[:count]
	}
	for index, operand := range f.slotOperands.Inputs[start:f.slotInput] {
		var value vmValue
		switch operand.Kind {
		case ir.OperandConstant:
			var err error
			value, err = f.module.constantValueAt(int(operand.Index))
			if err != nil {
				return nil, err
			}
		case ir.OperandLocal:
			value = f.localCells[operand.Index].load()
		default:
			value = f.slotValue(int(operand.Index))
		}
		f.popValues[index] = value
	}
	f.slotInput = start
	return f.popValues, nil
}

func (f *frame) takeCallOperands(count int) ([]ir.Operand, error) {
	if f == nil || f.slotOperands == nil || count < 0 || count > f.slotInput {
		return nil, errors.New("operand underflow")
	}
	start := f.slotInput - count
	operands := f.slotOperands.Inputs[start:f.slotInput]
	f.slotInput = start
	return operands, nil
}

func (f *frame) releasePopValues() {
	clear(f.popValues)
	f.popValues = f.popValues[:0]
}

func (f *frame) retainReturnValues(values []vmValue) []vmValue {
	clear(f.returnValues)
	f.returnValues = append(f.returnValues[:0], values...)
	return f.returnValues
}

func (f *frame) pop2() (vmValue, vmValue, error) {
	right, err := f.pop()
	if err != nil {
		return vmValue{}, vmValue{}, err
	}
	left, err := f.pop()
	return left, right, err
}

func (f *frame) address(payload ir.AddressPayload) (vmValue, error) {
	slot, err := f.capture(payload)
	if err != nil {
		return vmValue{}, err
	}
	return f.slotAddress(slot, payload)
}

func (f *frame) slotAddress(slot *slot, payload ir.AddressPayload) (vmValue, error) {
	if slot == nil {
		return vmValue{}, errors.New("address target has no storage")
	}
	if len(payload.Path) != 0 {
		return f.pathAddress(slot, payload)
	}
	pointerType := slot.typ.String()
	if pointerType == "" {
		pointerType = slot.load().Type.String()
	}
	if slot.module != nil {
		pointerType = slot.module.qualifyLocalType(pointerType)
	}
	return newSlotPointerValue(pointerType, fmt.Sprintf("slot:%p", slot), slot), nil
}

func (f *frame) pathAddress(root *slot, payload ir.AddressPayload) (vmValue, error) {
	if root.module != nil && root.module.vm != nil && root.module.vm.heap != nil {
		if _, err := root.ensureManagedCell(); err != nil {
			return vmValue{}, err
		}
	}
	current := root.load()
	identity := fmt.Sprintf("slot:%p", root)
	path := payload.Path
	var backingRoot vmValue
	var indexes []vmValue
	for position, segment := range payload.Path {
		switch segment.Kind {
		case "indirect":
			pointer, err := pointerValue(current)
			if err != nil {
				return vmValue{}, err
			}
			if pointer.Identity != "" {
				identity = pointer.Identity
			}
			current, err = derefPointer(current)
			if err != nil {
				return vmValue{}, err
			}
		case "field":
			var err error
			current, err = loadFieldValue(root.module, current, segment.Field)
			if err != nil {
				return vmValue{}, err
			}
			identity += ".field:" + segment.Field
		case "index":
			local, ok := f.function.LocalIndexes[segment.Local]
			if !ok || local < 0 || local >= len(f.localCells) || f.localCells[local] == nil {
				return vmValue{}, fmt.Errorf("unknown address path index local %q", segment.Local)
			}
			index := f.localCells[local].load()
			var err error
			indexes = append(indexes, index)
			indexed := current
			if indexed.Type.ShapeKind() == types.Pointer {
				indexed, err = derefPointer(indexed)
				if err != nil {
					return vmValue{}, err
				}
			}
			current, err = indexValue(root.module, current, index)
			if err != nil {
				return vmValue{}, err
			}
			var slice *vmSlice
			switch data := indexed.payload().(type) {
			case *vmSlice:
				slice = data
			case *vmArray:
				slice = &data.vmSlice
			}
			if slice != nil {
				i, err := asInt64(index)
				if err != nil {
					return vmValue{}, err
				}
				identity = slice.elementIdentity(i)
				// Element addresses capture the backing, not the variable that
				// currently contains the slice header.
				backingRoot = indexed
				path = payload.Path[position:]
				indexes = []vmValue{index}
			} else {
				identity += fmt.Sprintf(".index:%v", index.materializedData())
			}
		default:
			return vmValue{}, fmt.Errorf("unsupported address path segment kind %q", segment.Kind)
		}
	}
	if backingRoot.Type.Valid() {
		return newTargetPointer(&vmPointer{
			Type: current.Type, Identity: identity, target: pointerBackingPath,
			module: root.module, parent: backingRoot, path: path, indexes: indexes,
		}), nil
	}
	return newPathPointerValue(root.module.qualifyLocalType(current.Type.String()), identity, root, path, indexes), nil
}

func loadAddressPath(module *moduleInstance, root vmValue, path []ir.AddressPathSegment, indexes []vmValue) (vmValue, error) {
	current := root
	indexPosition := 0
	for _, segment := range path {
		switch segment.Kind {
		case "indirect":
			value, err := derefPointer(current)
			if err != nil {
				return vmValue{}, err
			}
			current = value
		case "field":
			value, err := loadFieldValue(module, current, segment.Field)
			if err != nil {
				return vmValue{}, err
			}
			current = value
		case "index":
			if indexPosition >= len(indexes) {
				return vmValue{}, errors.New("address path index value is missing")
			}
			value, err := indexValue(module, current, indexes[indexPosition])
			if err != nil {
				return vmValue{}, err
			}
			indexPosition++
			current = value
		default:
			return vmValue{}, fmt.Errorf("unsupported address path segment kind %q", segment.Kind)
		}
	}
	return current, nil
}

func storeAddressPathMode(module *moduleInstance, root vmValue, path []ir.AddressPathSegment, indexes []vmValue, value vmValue, cloneLeaf bool) (vmValue, error) {
	if len(path) == 0 {
		return value, nil
	}
	segment := path[0]
	if len(path) == 1 {
		switch segment.Kind {
		case "indirect":
			var err error
			if cloneLeaf {
				err = storePointer(root, value)
			} else {
				err = commitPointerMutation(root, value)
			}
			if err != nil {
				return vmValue{}, err
			}
			return root, nil
		case "field":
			updated, err := storeFieldValueMode(module, root, segment.Field, value, cloneLeaf)
			if err != nil {
				return vmValue{}, err
			}
			return updated, nil
		case "index":
			if len(indexes) == 0 {
				return vmValue{}, errors.New("address path index value is missing")
			}
			updated, err := setIndexValueMode(module, root, indexes[0], value, cloneLeaf)
			if err != nil {
				return vmValue{}, err
			}
			return updated, nil
		default:
			return vmValue{}, fmt.Errorf("unsupported address path segment kind %q", segment.Kind)
		}
	}
	switch segment.Kind {
	case "indirect":
		child, err := derefPointer(root)
		if err != nil {
			return vmValue{}, err
		}
		updated, err := storeAddressPathMode(module, child, path[1:], indexes, value, cloneLeaf)
		if err != nil {
			return vmValue{}, err
		}
		if err := commitPointerMutation(root, updated); err != nil {
			return vmValue{}, err
		}
		return root, nil
	case "field":
		child, err := loadFieldValue(module, root, segment.Field)
		if err != nil {
			return vmValue{}, err
		}
		updated, err := storeAddressPathMode(module, child, path[1:], indexes, value, cloneLeaf)
		if err != nil {
			return vmValue{}, err
		}
		root, err = storeFieldValueMode(module, root, segment.Field, updated, false)
		if err != nil {
			return vmValue{}, err
		}
		return root, nil
	case "index":
		if len(indexes) == 0 {
			return vmValue{}, errors.New("address path index value is missing")
		}
		index := indexes[0]
		child, err := indexValue(module, root, index)
		if err != nil {
			return vmValue{}, err
		}
		updated, err := storeAddressPathMode(module, child, path[1:], indexes[1:], value, cloneLeaf)
		if err != nil {
			return vmValue{}, err
		}
		root, err = setIndexValueMode(module, root, index, updated, false)
		if err != nil {
			return vmValue{}, err
		}
		return root, nil
	default:
		return vmValue{}, fmt.Errorf("unsupported address path segment kind %q", segment.Kind)
	}
}

func (f *frame) capture(payload ir.AddressPayload) (*slot, error) {
	switch payload.Kind {
	case "local":
		index, ok := f.function.LocalIndexes[payload.Local]
		if !ok || index < 0 || index >= len(f.localCells) || f.localCells[index] == nil {
			return nil, fmt.Errorf("unknown local %q", payload.Local)
		}
		return f.localCells[index], nil
	case "upvalue":
		index, ok := f.function.UpvalueIndexes[payload.Upvalue]
		if !ok || index < 0 || index >= len(f.upvalueCells) || f.upvalueCells[index] == nil {
			return nil, fmt.Errorf("unknown upvalue slot %q", payload.Upvalue)
		}
		return f.upvalueCells[index], nil
	case "global":
		index, ok := f.module.executable.Globals[payload.Global]
		if !ok || index < 0 || index >= len(f.module.globalCells) || f.module.globalCells[index] == nil {
			return nil, fmt.Errorf("unknown global %q", payload.Global)
		}
		return f.module.globalCells[index], nil
	default:
		return nil, fmt.Errorf("unsupported address kind %q", payload.Kind)
	}
}
