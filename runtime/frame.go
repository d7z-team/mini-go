package runtime

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type frame struct {
	execution          executionFrame
	revision           *instanceRevision
	module             *moduleInstance
	function           loadedFunction
	executionContextID int64
	pc                 int
	localCells         []*slot
	localStorage       []slot
	upvalueCells       []*slot
	stack              []vmValue
	popValues          []vmValue
	returnValues       []vmValue
	defers             []deferredCall
	mapIterators       map[string]*mapIterator
	idleCapacity       frameCapacity
	idlePhysicalBytes  int64
}

// frameCapacity retains the guest capacity ledger when idle buffers are evicted.
type frameCapacity struct {
	locals, upvalues, stack, popped, returned, defers int
}

type slot struct {
	mu          *sync.Mutex
	typ         vmType
	variadic    bool
	module      *moduleInstance
	value       vmValue
	initialized bool
}

const (
	maxIdleFramesPerFunction   = 8
	maxIdleFrameBytesPerModule = 8 << 20
	maxPhysicalFrameCacheBytes = 8 << 20
)

func newSlot(typ vmType, module *moduleInstance, variadic bool) *slot {
	return &slot{mu: new(sync.Mutex), typ: typ, variadic: variadic, module: module}
}

// snapshot does not materialize a zero value, so tracing never creates roots.
func (s *slot) snapshot() (vmValue, bool) {
	if s == nil {
		return vmValue{}, false
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
	zero := s.module.zeroValue(s.typ)
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

func (s *slot) store(value vmValue) error {
	if s == nil {
		return errors.New("nil slot")
	}
	if s.typ.Ref.Kind == types.Any {
		normalized, err := s.module.coerceAssignableRuntimeValue(value, s.typ, false)
		if err != nil {
			return err
		}
		s.publish(s.module.cloneValueForStore(normalized))
		return nil
	}
	if s.typ.Equal(value.Type) {
		value.Type = s.typ
		switch s.typ.Ref.Kind {
		case types.Void, types.Primitive, types.Map, types.Pointer, types.Waitable, types.Function:
			s.publish(value)
			return nil
		}
	}
	normalized, err := s.module.coerceAssignableRuntimeValue(value, s.typ, s.variadic)
	if err != nil {
		return err
	}
	prepared := s.module.cloneValueForStore(normalized)
	current, _ := s.snapshot()
	s.publish(s.module.assignPreparedValue(current, prepared))
	return nil
}

func newFrame(module *moduleInstance, function loadedFunction, args []vmValue, captured map[string]*slot, executionContextID int64) (*frame, bool, error) {
	var callFrame *frame
	allocated := true
	module.framePoolMu.Lock()
	pool := module.framePools[function.Decl.ID]
	if len(pool) != 0 {
		last := len(pool) - 1
		callFrame = pool[last]
		pool[last] = nil
		module.framePools[function.Decl.ID] = pool[:last]
		module.framePoolBytes -= callFrame.logicalBytes()
		module.framePoolPhysicalBytes -= callFrame.idlePhysicalBytes
		if module.vm != nil {
			module.vm.idleFrameBytes.Add(-callFrame.idlePhysicalBytes)
		}
		callFrame.idlePhysicalBytes = 0
		if module.framePoolBytes < 0 {
			module.framePoolBytes = 0
		}
		allocated = false
	}
	module.framePoolMu.Unlock()
	if callFrame == nil {
		callFrame = &frame{idleCapacity: frameCapacity{
			locals: len(function.Decl.Locals), upvalues: len(function.Decl.Upvalues), stack: function.MaxStack,
		}}
	}
	if callFrame.localStorage == nil {
		capacity := callFrame.idleCapacity
		callFrame.localStorage = make([]slot, capacity.locals)
		callFrame.localCells = make([]*slot, capacity.locals)
		for i := range callFrame.localStorage {
			callFrame.localStorage[i] = slot{typ: function.LocalTypes[i], module: module, variadic: function.LocalVariadic[i]}
			callFrame.localCells[i] = &callFrame.localStorage[i]
		}
		callFrame.upvalueCells = make([]*slot, capacity.upvalues)
		callFrame.stack = make([]vmValue, 0, capacity.stack)
		callFrame.popValues = make([]vmValue, 0, capacity.popped)
		callFrame.returnValues = make([]vmValue, 0, capacity.returned)
		callFrame.defers = make([]deferredCall, 0, capacity.defers)
		callFrame.idleCapacity = frameCapacity{}
	}
	callFrame.revision = module.revision
	callFrame.module = module
	callFrame.function = function
	callFrame.executionContextID = executionContextID
	callFrame.pc = 0
	callFrame.stack = callFrame.stack[:0]
	callFrame.defers = callFrame.defers[:0]
	for i, local := range function.Decl.Locals {
		cell := &callFrame.localStorage[i]
		if i < len(function.LocalEscapes) && function.LocalEscapes[i] {
			cell = newSlot(function.LocalTypes[i], module, function.LocalVariadic[i])
		}
		callFrame.localCells[i] = cell
		if i < len(args) {
			if err := cell.store(args[i]); err != nil {
				callFrame.recycle()
				return nil, allocated, fmt.Errorf("argument %s: %w", local.ID, err)
			}
		}
	}
	for i, upvalue := range function.Decl.Upvalues {
		capturedSlot, ok := captured[upvalue.ID]
		if !ok || capturedSlot == nil {
			callFrame.recycle()
			return nil, allocated, fmt.Errorf("missing upvalue %q", upvalue.ID)
		}
		callFrame.upvalueCells[i] = capturedSlot
	}
	return callFrame, allocated, nil
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
	f.execution = executionFrame{}
	for i := range f.localCells {
		f.localStorage[i].value = vmValue{}
		f.localStorage[i].initialized = false
		f.localCells[i] = &f.localStorage[i]
	}
	for i := range f.upvalueCells {
		f.upvalueCells[i] = nil
	}
	// Every stack/defer truncation clears the released range. Only the live
	// high-water marks remain to clear here, irrespective of retained capacity.
	clear(f.stack)
	clear(f.popValues)
	clear(f.returnValues)
	clear(f.defers)
	f.mapIterators = nil
	f.stack = f.stack[:0]
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
		physicalBytes := int64(unsafe.Sizeof(*f)) +
			int64(cap(f.localStorage))*int64(unsafe.Sizeof(slot{})) +
			int64(cap(f.localCells)+cap(f.upvalueCells))*int64(unsafe.Sizeof((*slot)(nil))) +
			int64(cap(f.stack)+cap(f.popValues)+cap(f.returnValues))*int64(unsafe.Sizeof(vmValue{})) +
			int64(cap(f.defers))*int64(unsafe.Sizeof(deferredCall{}))
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
		} else {
			// Keep only the guest capacity ledger when physical storage is
			// evicted. A later hit restores these exact capacities without
			// changing guest allocation history.
			f.idleCapacity = frameCapacity{
				locals: cap(f.localStorage), upvalues: cap(f.upvalueCells), stack: cap(f.stack),
				popped: cap(f.popValues), returned: cap(f.returnValues), defers: cap(f.defers),
			}
			f.localCells = nil
			f.localStorage = nil
			f.upvalueCells = nil
			f.stack = nil
			f.popValues = nil
			f.returnValues = nil
			f.defers = nil
		}
		module.framePools[functionID] = append(pool, f)
		module.framePoolBytes += logicalBytes
		return
	}
	f.module = nil
	f.revision = nil
	f.function = loadedFunction{}
	f.localCells = nil
	f.localStorage = nil
	f.upvalueCells = nil
	f.stack = nil
	f.popValues = nil
	f.returnValues = nil
	f.defers = nil
}

func (f *frame) logicalBytes() int64 {
	if f == nil {
		return 0
	}
	if capacity := f.idleCapacity; capacity != (frameCapacity{}) {
		slots := capacity.locals + capacity.upvalues + capacity.stack + capacity.popped + capacity.returned + capacity.defers
		return ir.RuntimeNodeBytes + int64(slots)*ir.RuntimeSlotBytes
	}
	return ir.RuntimeNodeBytes + int64(cap(f.localStorage)+cap(f.upvalueCells)+cap(f.stack)+cap(f.popValues)+cap(f.returnValues)+cap(f.defers))*ir.RuntimeSlotBytes
}

func (f *frame) normalizeReturnValues(values []vmValue) ([]vmValue, error) {
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
		values[i] = f.module.cloneValueForStore(normalized)
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
		return f.normalizeReturnValues(values)
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
	return f.normalizeReturnValues(values)
}

func (f *frame) push(value vmValue) {
	f.stack = append(f.stack, value)
}

func (f *frame) pop() (vmValue, error) {
	values, err := f.popN(1)
	if err != nil {
		return vmValue{}, err
	}
	return values[0], nil
}

func (f *frame) popN(count int) ([]vmValue, error) {
	if count < 0 {
		return nil, fmt.Errorf("negative pop count %d", count)
	}
	if len(f.stack) < count {
		return nil, fmt.Errorf("stack underflow: need %d values, have %d", count, len(f.stack))
	}
	start := len(f.stack) - count
	clear(f.popValues)
	f.popValues = append(f.popValues[:0], f.stack[start:]...)
	clear(f.stack[start:])
	f.stack = f.stack[:start]
	return f.popValues, nil
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
	values, err := f.popN(2)
	if err != nil {
		return vmValue{}, vmValue{}, err
	}
	return values[0], values[1], nil
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
	current := root.load()
	identity := fmt.Sprintf("slot:%p", root)
	path := payload.Path
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
			current, err = indexValue(root.module, current, index)
			if err != nil {
				return vmValue{}, err
			}
			var slice *vmSlice
			switch data := indexed.Data.(type) {
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
				identity = fmt.Sprintf("slice:%p.index:%d", slice.vmSliceStorage, int64(slice.Start)+i)
				// Element addresses capture the backing, not the variable that
				// currently contains the slice header.
				root = &slot{typ: indexed.Type, module: root.module, value: indexed, initialized: true}
				path = payload.Path[position:]
				indexes = []vmValue{index}
			} else {
				identity += fmt.Sprintf(".index:%v", index.materializedData())
			}
		default:
			return vmValue{}, fmt.Errorf("unsupported address path segment kind %q", segment.Kind)
		}
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

func storeAddressPath(module *moduleInstance, root vmValue, path []ir.AddressPathSegment, indexes []vmValue, value vmValue) (vmValue, error) {
	return storeAddressPathMode(module, root, path, indexes, value, true)
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
