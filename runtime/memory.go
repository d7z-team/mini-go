package runtime

import (
	"math"

	artifact "github.com/d7z-team/mini-go/runtime/bytecode"
)

// refreshLiveGuestBytes replaces the conservative allocation delta with a
// deterministic census of mutable values still reachable by the VM owner.
func (vm *vm) refreshLiveGuestBytes() int64 {
	if vm == nil {
		return 0
	}
	sizer := newRuntimeValueSizer()
	for _, value := range vm.controlRoots {
		sizer.value(value)
	}
	sizer.add(vm.dynamicTypeBytes)
	if revision := vm.revision.Load(); revision != nil && revision.modules != nil {
		for _, module := range revision.modules.modules {
			if module == nil || module.state == nil {
				continue
			}
			for _, cell := range module.state.globals {
				sizer.slot(cell)
			}
		}
	}
	if machine := vm.machine; machine != nil {
		for _, task := range machine.tasks {
			sizer.task(task)
		}
	}
	for _, timer := range vm.timers {
		if timer != nil {
			sizer.value(timer.signal)
		}
	}
	for _, value := range vm.reflectTypeValues.snapshot() {
		sizer.value(value)
	}
	vm.liveGuestBytes.Store(sizer.bytes)
	vm.allocatedSinceSweep.Store(0)
	updateAtomicMaximum(&vm.peakGuestBytes, sizer.bytes)
	return sizer.bytes
}

type runtimeValueSizer struct {
	bytes          int64
	seenPointers   map[*vmPointer]bool
	seenSlices     map[*vmSlice]bool
	seenStorage    map[*vmSliceStorage]bool
	seenMaps       map[*vmMap]bool
	seenStructs    map[*vmStruct]bool
	seenSlots      map[*slot]bool
	seenCells      map[*slot]bool
	seenWaitables  map[*waitableResource]bool
	seenMutexes    map[*mutexResource]bool
	seenSelections map[*channelSelection]bool
	pendingValues  []vmValue
	walking        bool
}

func newRuntimeValueSizer() *runtimeValueSizer {
	return &runtimeValueSizer{
		seenPointers: make(map[*vmPointer]bool), seenSlices: make(map[*vmSlice]bool),
		seenStorage: make(map[*vmSliceStorage]bool),
		seenMaps:    make(map[*vmMap]bool), seenStructs: make(map[*vmStruct]bool),
		seenSlots: make(map[*slot]bool), seenWaitables: make(map[*waitableResource]bool),
		seenCells:      make(map[*slot]bool),
		seenMutexes:    make(map[*mutexResource]bool),
		seenSelections: make(map[*channelSelection]bool),
	}
}

func (sizer *runtimeValueSizer) add(bytes int64) {
	if bytes <= 0 || sizer.bytes == math.MaxInt64 {
		return
	}
	if bytes > math.MaxInt64-sizer.bytes {
		sizer.bytes = math.MaxInt64
		return
	}
	sizer.bytes += bytes
}

func (sizer *runtimeValueSizer) slot(cell *slot) {
	if cell == nil || sizer.seenSlots[cell] {
		return
	}
	sizer.seenSlots[cell] = true
	if value, initialized := cell.snapshot(); initialized {
		sizer.value(value)
	}
}

func (sizer *runtimeValueSizer) value(value vmValue) {
	sizer.pendingValues = append(sizer.pendingValues, value)
	if sizer.walking {
		return
	}
	sizer.walking = true
	for len(sizer.pendingValues) != 0 {
		last := len(sizer.pendingValues) - 1
		value = sizer.pendingValues[last]
		sizer.pendingValues[last] = vmValue{}
		sizer.pendingValues = sizer.pendingValues[:last]
		sizer.visitValue(value)
	}
	sizer.walking = false
}

func (sizer *runtimeValueSizer) visitValue(value vmValue) {
	switch data := value.Data.(type) {
	case string:
		sizer.add(int64(len(data)) * artifact.RuntimeByteBytes)
	case functionRef:
		sizer.add(artifact.RuntimeNodeBytes + int64(len(data.upvalues))*artifact.RuntimeSlotBytes)
		for _, cell := range data.upvalues {
			sizer.slot(cell)
		}
	case reflectMethodTarget:
		sizer.add(artifact.RuntimeNodeBytes)
		sizer.value(data.Receiver)
	case reflectUnboundMethodTarget:
		sizer.add(artifact.RuntimeNodeBytes)
	case reflectMakeFuncTarget:
		sizer.add(artifact.RuntimeNodeBytes)
		sizer.value(newVMValue(data.functionType, data.handler))
	case vmValue:
		sizer.add(artifact.RuntimeNodeBytes)
		sizer.value(data)
	case *vmPointer:
		if data == nil || sizer.seenPointers[data] {
			return
		}
		sizer.seenPointers[data] = true
		sizer.add(artifact.RuntimeNodeBytes + int64(len(data.path)+len(data.indexes))*artifact.RuntimeSlotBytes)
		if data.original != nil {
			sizer.value(newVMValue(pointerType(data.original.Type), data.original))
			return
		}
		sizer.slot(data.slot)
		for _, index := range data.indexes {
			sizer.value(index)
		}
		if data.cell != nil && !sizer.seenCells[data.cell] {
			sizer.seenCells[data.cell] = true
			value, _ := data.cell.snapshot()
			sizer.value(value)
		}
		if data.parent.Type.Valid() {
			sizer.value(data.parent)
		}
		if data.array != nil {
			sizer.value(newVMValue("", data.array))
		}
	case *vmSlice:
		if data == nil || sizer.seenSlices[data] {
			return
		}
		sizer.seenSlices[data] = true
		sizer.add(artifact.RuntimeNodeBytes)
		if data.vmSliceStorage != nil && sizer.seenStorage[data.vmSliceStorage] {
			return
		}
		if data.vmSliceStorage != nil {
			sizer.seenStorage[data.vmSliceStorage] = true
		}
		if data.ByteBacked {
			sizer.add(int64(cap(data.ByteBacking)) * artifact.RuntimeByteBytes)
			return
		}
		sizer.add(int64(cap(data.Backing)) * artifact.RuntimeSlotBytes)
		for _, item := range data.valuesSnapshot() {
			sizer.value(item)
		}
	case *vmMap:
		if data == nil || sizer.seenMaps[data] {
			return
		}
		sizer.seenMaps[data] = true
		entries := data.snapshot()
		sizer.add(artifact.RuntimeNodeBytes + int64(len(entries))*artifact.RuntimeMapEntryBytes)
		for _, entry := range entries {
			sizer.value(entry.Key)
			sizer.value(entry.Value)
		}
	case *vmArray:
		sizer.value(newVMValue(value.Type, &data.vmSlice))
	case *vmStruct:
		if data == nil || sizer.seenStructs[data] {
			return
		}
		sizer.seenStructs[data] = true
		values, _ := data.snapshot()
		sizer.add(artifact.RuntimeNodeBytes + int64(cap(values))*artifact.RuntimeSlotBytes)
		for _, field := range values {
			if field.Type.Valid() {
				sizer.value(field)
			}
		}
	case *channelSelection:
		if data == nil || sizer.seenSelections[data] {
			return
		}
		sizer.seenSelections[data] = true
		if data.allocated {
			sizer.add(artifact.RuntimeNodeBytes + int64(len(data.cases))*(artifact.RuntimeNodeBytes+3*artifact.RuntimeSlotBytes))
		}
		sizer.value(data.value)
		for _, selected := range data.cases {
			sizer.value(selected.channel)
			sizer.value(selected.value)
			sizer.value(selected.zero)
		}
	case *mutexResource:
		if data == nil || sizer.seenMutexes[data] {
			return
		}
		sizer.seenMutexes[data] = true
		sizer.add(artifact.RuntimeNodeBytes)
		if data.grant != nil {
			sizer.add(artifact.RuntimeNodeBytes + artifact.RuntimeSlotBytes)
		}
		for waiter := data.head; waiter != nil; waiter = waiter.next {
			sizer.add(artifact.RuntimeNodeBytes + artifact.RuntimeSlotBytes)
		}
	case *waitableResource:
		if data == nil || sizer.seenWaitables[data] {
			return
		}
		sizer.seenWaitables[data] = true
		for selected := data.selectHead; selected != nil; selected = selected.next {
			sizer.value(newVMValue("Any", selected.selection))
		}
		sizer.add(artifact.RuntimeNodeBytes + int64(cap(data.Buffer))*artifact.RuntimeSlotBytes)
		for _, item := range data.Buffer[data.bufferHead:] {
			sizer.value(item)
		}
	}
}

func (sizer *runtimeValueSizer) task(task *executionTask) {
	if task == nil {
		return
	}
	sizer.value(newVMValue("Any", task.preparingSelection))
	for _, value := range task.sliceValues {
		sizer.value(value)
	}
	for _, active := range task.retainedFrames {
		if active == nil || active.frame == nil {
			continue
		}
		frame := active.frame
		for _, iterator := range frame.mapIterators {
			sizer.add(iterator.logicalBytes())
			sizer.value(iterator.object)
		}
		sizer.add(artifact.RuntimeNodeBytes + int64(cap(frame.localStorage)+cap(frame.upvalueCells)+cap(frame.stack)+cap(frame.popValues)+cap(frame.returnValues))*artifact.RuntimeSlotBytes)
		for _, cell := range frame.localCells {
			sizer.slot(cell)
		}
		for _, cell := range frame.upvalueCells {
			sizer.slot(cell)
		}
		for _, value := range frame.stack {
			sizer.value(value)
		}
		for _, value := range frame.popValues {
			sizer.value(value)
		}
		for _, value := range frame.returnValues {
			sizer.value(value)
		}
		for _, deferred := range frame.defers {
			sizer.value(newVMValue("Function", deferred.ref))
		}
		if active.completion != nil {
			for _, value := range active.completion.returnValues {
				sizer.value(value)
			}
			if active.completion.panic != nil {
				sizer.value(active.completion.panic.err.Value)
			}
		}
		if active.recoveredPanic != nil {
			sizer.value(active.recoveredPanic.err.Value)
		}
	}
	if blocked := task.blocked; blocked != nil {
		sizer.value(newVMValue("Any", blocked.selection))
		if blocked.mutex != nil {
			sizer.value(newVMValue("Waitable<Bool>", blocked.mutex.resource))
		}
	}
}
