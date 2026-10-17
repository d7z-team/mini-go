package runtime

import (
	"encoding/binary"
	"errors"
)

type managedFunctionRecord struct {
	modulePath   string
	functionID   string
	exact        *moduleInstance
	kind         uint8
	method       TypeMethodInfo
	targetModule *moduleInstance
	// upvalueTypes follows the immutable function declaration order. The page
	// owns the guest reachability edges; functionRef only owns the live slots.
	upvalueTypes []vmType
}

func (module *moduleInstance) newManagedFunction(typ vmType, payload any, batch *managedAllocationBatch) (vmValue, error) {
	if module == nil || module.vm == nil {
		return vmValue{}, errors.New("managed function has no owner")
	}
	if module.vm.heap == nil {
		module.vm.heap = newManagedHeap()
	}
	if module.vm.valueLayouts == nil {
		module.vm.valueLayouts = newPreparedLayoutRegistry()
	}
	var ref functionRef
	var receiver vmValue
	var record managedFunctionRecord
	switch value := payload.(type) {
	case functionRef:
		ref = value
	case reflectMethodTarget:
		record.kind, record.method, record.targetModule = 1, value.Method, value.Module
		receiver = value.Receiver
	case reflectUnboundMethodTarget:
		record.kind, record.method, record.targetModule = 2, value.Method, value.Module
	case reflectMakeFuncTarget:
		record.kind, record.targetModule = 3, value.handlerModule
		ref = value.handler
	default:
		return vmValue{}, errors.New("invalid managed callable payload")
	}
	record.modulePath, record.functionID, record.exact = ref.ModulePath, ref.FunctionID, ref.exact
	var codeAnchor guestRef
	codeModule := record.exact
	if record.targetModule != nil {
		codeModule = record.targetModule
	}
	if codeModule != nil && codeModule.revision != nil {
		var err error
		codeAnchor, err = codeModule.revision.ensureManagedAnchor()
		if err != nil {
			return vmValue{}, err
		}
	}
	var captures []guestRef
	if len(ref.captureSlots) != 0 {
		captures = make([]guestRef, 0, len(ref.captureSlots))
		for _, slot := range ref.captureSlots {
			cell, err := slot.ensureManagedCell(batch)
			if err != nil {
				return vmValue{}, err
			}
			captures = append(captures, cell.ref)
			record.upvalueTypes = append(record.upvalueTypes, cell.typ)
		}
	}
	base := uint32(8)
	var dynamics []uint32
	var receiverBytes [24]byte
	if record.kind == 1 {
		encoded, err := module.dynamicEncodingWithBatch(receiver, map[uintptr]guestRef{}, batch)
		if err != nil {
			return vmValue{}, err
		}
		layout, err := module.vm.valueLayouts.layoutFor(encoded.typ)
		if err != nil {
			return vmValue{}, err
		}
		if err := encodeManagedDynamic(receiverBytes[:], layout.ID, encoded); err != nil {
			return vmValue{}, err
		}
		base, dynamics = 32, []uint32{8}
	}
	stride := max(uint32(8), base+uint32(len(record.upvalueTypes)*8))
	references := make([]uint32, len(record.upvalueTypes)+1)
	for index := range record.upvalueTypes {
		references[index+1] = base + uint32(index*8)
	}
	layoutID, err := module.vm.heap.registerLayout(managedLayout{stride: stride, references: references, dynamics: dynamics})
	if err != nil {
		return vmValue{}, err
	}
	defer module.vm.heap.releaseLayout(layoutID)
	reservation, err := module.vm.heap.reserveWithBatch(batch, layoutID, 1, 16+uint64(stride))
	if err != nil {
		return vmValue{}, err
	}
	binary.LittleEndian.PutUint32(reservation.bytes(), codeAnchor.slot)
	binary.LittleEndian.PutUint32(reservation.bytes()[4:], codeAnchor.generation)
	if record.kind == 1 {
		copy(reservation.bytes()[8:], receiverBytes[:])
	}
	for index, child := range captures {
		offset := int(base) + index*8
		binary.LittleEndian.PutUint32(reservation.bytes()[offset:], child.slot)
		binary.LittleEndian.PutUint32(reservation.bytes()[offset+4:], child.generation)
	}
	ticket := reservation.commit()
	if !ticket.valid() {
		return vmValue{}, errors.New("managed function publication failed")
	}
	module.vm.managedFunctionsMu.Lock()
	if module.vm.managedFunctions == nil {
		module.vm.managedFunctions = make(map[guestRef]managedFunctionRecord)
	}
	module.vm.managedFunctions[ticket] = record
	module.vm.managedFunctionsMu.Unlock()
	return vmValue{Type: typ, ref: ticket, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}, nil
}

func materializeManagedFunction(value vmValue) (any, bool) {
	if value.owner == nil || value.heap == nil || !value.ref.valid() {
		return functionRef{}, false
	}
	value.owner.managedFunctionsMu.Lock()
	record, ok := value.owner.managedFunctions[value.ref]
	value.owner.managedFunctionsMu.Unlock()
	if !ok {
		return functionRef{}, false
	}
	ref := functionRef{ModulePath: record.modulePath, FunctionID: record.functionID, exact: record.exact}
	base := uint32(8)
	if record.kind == 1 {
		base = 32
	}
	if len(record.upvalueTypes) != 0 {
		ref.captureSlots = make([]*slot, len(record.upvalueTypes))
		for index, typ := range record.upvalueTypes {
			cellRef, err := value.heap.readReference(value.ref, base+uint32(index*8))
			if err != nil {
				return nil, false
			}
			layout, err := value.layouts.layoutFor(typ)
			if err != nil {
				return nil, false
			}
			module := record.exact
			if module == nil {
				module = value.owner.rootModule()
			}
			cell := &slot{typ: typ, module: module, managedCell: &managedCellStorage{
				heap: value.heap, ref: cellRef, typ: typ, layout: layout, owner: value.owner,
			}}
			ref.captureSlots[index] = cell
		}
	}
	switch record.kind {
	case 1:
		var raw [24]byte
		if err := value.heap.read(value.ref, 8, raw[:]); err != nil {
			return nil, false
		}
		encoded, err := decodeManagedDynamicWithRegistry(raw[:], predeclaredRuntimeTypes["Any"], value.heap, value.layouts)
		if err != nil {
			return nil, false
		}
		return reflectMethodTarget{Receiver: decodeManagedValueWithLayouts(encoded, value.heap, value.layouts, value.owner), Method: record.method, Module: record.targetModule}, true
	case 2:
		return reflectUnboundMethodTarget{Method: record.method, Module: record.targetModule}, true
	case 3:
		info, ok := value.Type.FunctionInfo()
		if !ok {
			return nil, false
		}
		underlying := value.Type.Underlying()
		params := make([]vmType, len(info.Signature.Params))
		results := make([]vmType, len(info.Signature.Results))
		for index, param := range info.Signature.Params {
			params[index] = underlying.derived(param.Type)
		}
		for index, result := range info.Signature.Results {
			results[index] = underlying.derived(result)
		}
		return reflectMakeFuncTarget{functionType: value.Type, params: params, results: results, variadic: info.Signature.Variadic, handler: ref, handlerModule: record.targetModule}, true
	}
	return ref, true
}
