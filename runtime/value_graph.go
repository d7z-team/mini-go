package runtime

import ir "github.com/d7z-team/mini-go/runtime/bytecode"

type runtimeValueWalker struct {
	visitRevision  func(*instanceRevision)
	enter          func(vmValue) bool
	leave          func()
	stopped        bool
	seenPointers   map[*vmPointer]pointerVisit
	seenSlices     map[*vmSlice]bool
	seenMaps       map[*vmMap]bool
	seenStructs    map[*vmStruct]bool
	seenSlots      map[*slot]bool
	seenWaitables  map[*waitableResource]bool
	seenSelections map[*channelSelection]bool
}

type pointerVisit uint8

const (
	pointerVisited pointerVisit = 1 << iota
	pointerReading
)

func newRuntimeValueWalker(visitRevision func(*instanceRevision)) *runtimeValueWalker {
	return &runtimeValueWalker{
		visitRevision: visitRevision,
		seenPointers:  make(map[*vmPointer]pointerVisit), seenSlices: make(map[*vmSlice]bool),
		seenMaps: make(map[*vmMap]bool), seenSlots: make(map[*slot]bool),
		seenStructs:    make(map[*vmStruct]bool),
		seenWaitables:  make(map[*waitableResource]bool),
		seenSelections: make(map[*channelSelection]bool),
	}
}

func (walker *runtimeValueWalker) slot(cell *slot) {
	if walker.enter != nil {
		if walker.stopped || !walker.enter(vmValue{}) {
			return
		}
		defer walker.leave()
	}
	if walker.stopped || cell == nil || walker.seenSlots[cell] {
		return
	}
	walker.seenSlots[cell] = true
	value, initialized := cell.snapshot()
	if !initialized {
		return
	}
	walker.value(value)
}

func (walker *runtimeValueWalker) value(value vmValue) {
	if walker.stopped {
		return
	}
	if walker.enter != nil {
		if !walker.enter(value) {
			return
		}
		defer walker.leave()
	}
	switch data := value.Data.(type) {
	case *channelSelection:
		if data == nil || walker.seenSelections[data] {
			return
		}
		walker.seenSelections[data] = true
		walker.value(data.value)
		for _, selected := range data.cases {
			if walker.stopped {
				return
			}
			walker.value(selected.channel)
			walker.value(selected.value)
			walker.value(selected.zero)
		}
	case functionRef:
		if data.exact != nil {
			walker.visitRevision(data.exact.revision)
		}
		for _, cell := range data.upvalues {
			if walker.stopped {
				return
			}
			walker.slot(cell)
		}
	case reflectMethodTarget:
		if data.Module != nil {
			walker.visitRevision(data.Module.revision)
		}
		walker.value(data.Receiver)
	case reflectUnboundMethodTarget:
		if data.Module != nil {
			walker.visitRevision(data.Module.revision)
		}
	case reflectMakeFuncTarget:
		if data.handlerModule != nil {
			walker.visitRevision(data.handlerModule.revision)
		}
		walker.value(newVMValue(data.functionType, data.handler))
	case vmValue:
		walker.value(data)
	case *vmPointer:
		if data != nil && walker.seenPointers[data]&pointerVisited == 0 {
			walker.seenPointers[data] |= pointerVisited
			if data.original != nil {
				walker.value(newVMValue(pointerType(data.original.Type), data.original))
				return
			}
			if pointed := walker.pointerContents(data); pointed.Type.Valid() || pointed.Data != nil {
				walker.value(pointed)
			}
		}
	case *vmSlice:
		if data != nil && !walker.seenSlices[data] {
			walker.seenSlices[data] = true
			for _, item := range data.valuesSnapshot() {
				if walker.stopped {
					return
				}
				walker.value(item)
			}
		}
	case *vmMap:
		if data != nil && !walker.seenMaps[data] {
			walker.seenMaps[data] = true
			for _, entry := range data.snapshot() {
				if walker.stopped {
					return
				}
				walker.value(entry.Key)
				walker.value(entry.Value)
			}
		}
	case *vmArray:
		if data.ByteBacked {
			return
		}
		values := data.values()
		for _, item := range values {
			if walker.stopped {
				return
			}
			walker.value(item)
		}
	case *vmStruct:
		if data != nil && !walker.seenStructs[data] {
			walker.seenStructs[data] = true
			values, _ := data.snapshot()
			for _, field := range values {
				if walker.stopped {
					return
				}
				if field.Type.Valid() {
					walker.value(field)
				}
			}
		}
	case *waitableResource:
		if data != nil && !walker.seenWaitables[data] {
			walker.seenWaitables[data] = true
			for selected := data.selectHead; selected != nil; selected = selected.next {
				if walker.stopped {
					return
				}
				walker.value(newVMValue("Any", selected.selection))
			}
			for _, item := range data.Buffer[data.bufferHead:] {
				if walker.stopped {
					return
				}
				walker.value(item)
			}
		}
	}
}

// pointerContents observes stored references without initializing slots, coercing
// values or expanding byte-backed array views. Storage accounting follows owners
// separately; revision inspection follows the selected address only.
func (walker *runtimeValueWalker) pointerContents(pointer *vmPointer) vmValue {
	if pointer == nil || walker.stopped || walker.seenPointers[pointer]&pointerReading != 0 {
		return vmValue{}
	}
	walker.seenPointers[pointer] |= pointerReading
	defer func() {
		walker.seenPointers[pointer] &^= pointerReading
		if walker.seenPointers[pointer] == 0 {
			delete(walker.seenPointers, pointer)
		}
	}()
	if pointer.original != nil {
		return walker.addressContents(newVMValue("", pointer.original), nil, nil, true)
	}
	switch pointer.target {
	case pointerCell:
		if pointer.cell != nil {
			value, _ := pointer.cell.snapshot()
			return value
		}
	case pointerField:
		return walker.addressContents(pointer.parent, []ir.AddressPathSegment{{Kind: "field", Field: pointer.field}}, nil, true)
	case pointerIndex:
		return walker.addressContents(pointer.parent, []ir.AddressPathSegment{{Kind: "index"}}, []vmValue{newVMValue("Int", pointer.index)}, true)
	case pointerArray:
		if pointer.array != nil && !pointer.array.ByteBacked {
			return newVMValue(pointer.Type, &vmArray{vmSlice: vmSlice{
				vmSliceStorage: pointer.array.vmSliceStorage,
				Start:          pointer.array.Start, Len: pointer.arrayLen, Cap: pointer.arrayLen,
			}})
		}
	case pointerSlot:
		if value, initialized := pointer.slot.snapshot(); initialized {
			return walker.addressContents(value, pointer.path, pointer.indexes, false)
		}
	}
	return vmValue{}
}

func (walker *runtimeValueWalker) addressContents(value vmValue, path []ir.AddressPathSegment, indexes []vmValue, indirect bool) vmValue {
	indexPosition := 0
	for position := 0; ; position++ {
		if walker.stopped {
			return vmValue{}
		}
		if position < len(path) {
			if walker.enter != nil {
				if !walker.enter(value) {
					return vmValue{}
				}
				defer walker.leave()
			}
			indirect = true
		}
		if pointer, ok := value.Data.(*vmPointer); ok && indirect {
			if walker.enter != nil {
				if !walker.enter(value) {
					return vmValue{}
				}
				defer walker.leave()
			}
			value = walker.pointerContents(pointer)
		}
		if position == len(path) {
			return value
		}
		indirect = false
		segment := path[position]
		switch segment.Kind {
		case "indirect":
		case "field":
			value, _ = structValueField(value.Data, segment.Field)
		case "index":
			if indexPosition >= len(indexes) {
				return vmValue{}
			}
			index, err := asInt64(indexes[indexPosition])
			indexPosition++
			if err != nil || index < 0 {
				return vmValue{}
			}
			switch data := value.Data.(type) {
			case *vmSlice:
				if data == nil || data.ByteBacked || index >= int64(data.Len) {
					return vmValue{}
				}
				value = data.valueAt(int(index))
			case *vmArray:
				if index >= int64(data.Len) || data.ByteBacked {
					return vmValue{}
				}
				value = data.valueAt(int(index))
			default:
				return vmValue{}
			}
		default:
			return vmValue{}
		}
	}
}
