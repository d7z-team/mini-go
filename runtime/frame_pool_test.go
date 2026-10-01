package runtime

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestConcurrentFramePoolKeepsPrivateInvocationState(t *testing.T) {
	function := loadedFunction{
		Decl:       ir.Function{ID: "fn.concurrent", Locals: []ir.Local{{ID: "value", Type: types.Builtin(types.PrimitiveInt)}}, Code: &ir.SlotCode{Types: []types.TypeRef{types.Builtin(types.PrimitiveInt)}}},
		LocalTypes: []vmType{predeclaredRuntimeTypes["Int"]}, LocalVariadic: []bool{false},
	}
	module := &moduleInstance{vm: &vm{}}
	var group sync.WaitGroup
	var mu sync.Mutex
	active := make(map[*frame]bool)
	for worker := range 4 {
		group.Go(func() {
			for sequence := range 100 {
				id := int64(worker*100 + sequence)
				invocation, _, err := newFrame(module, function, []vmValue{newVMValue("Int", id)}, nil, id)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if active[invocation] {
					t.Error("frame leased to multiple invocations")
				}
				active[invocation] = true
				mu.Unlock()
				invocation.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
				invocation.push(invocation.localCells[0].load())
				invocation.beginSlotInstruction(1, &preparedInstruction{operands: &ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}}})
				result, err := invocation.pop()
				if err != nil || result.materializedData() != id {
					t.Errorf("invocation %d: result=%v err=%v", id, result, err)
				}
				mu.Lock()
				delete(active, invocation)
				mu.Unlock()
				invocation.recycle()
			}
		})
	}
	group.Wait()
	if module.framePoolPhysicalBytes != module.vm.idleFrameBytes.Load() {
		t.Fatal("frame pool physical accounting differs from instance accounting")
	}
}

func TestFramePhysicalEvictionPreservesGuestCapacity(t *testing.T) {
	function := loadedFunction{
		Decl:          ir.Function{ID: "fn.physical", Locals: []ir.Local{{ID: "value", Type: types.Builtin(types.PrimitiveInt)}}, Code: &ir.SlotCode{Types: make([]types.TypeRef, maxPhysicalFrameCacheBytes/int(unsafe.Sizeof(vmValue{}))+1)}},
		LocalTypes:    []vmType{predeclaredRuntimeTypes["Int"]},
		LocalVariadic: []bool{false},
	}
	module := &moduleInstance{vm: &vm{}}
	first, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(7))}, nil, 1)
	if err != nil || !allocated {
		t.Fatalf("first frame: allocated=%v err=%v", allocated, err)
	}
	logical := first.logicalBytes()
	capacity := cap(first.slotValues)
	first.recycle()
	if module.framePoolBytes != logical || module.vm.idleFrameBytes.Load() != 0 {
		t.Fatalf("guest/physical pool accounting: %d/%d", module.framePoolBytes, module.vm.idleFrameBytes.Load())
	}
	if first.slotValues != nil {
		t.Fatal("oversized physical backing retained")
	}
	second, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(42))}, nil, 2)
	if err != nil || allocated {
		t.Fatalf("restored frame changed guest allocation: allocated=%v err=%v", allocated, err)
	}
	if cap(second.slotValues) != capacity || second.logicalBytes() != logical {
		t.Fatalf("restored capacities changed guest census: capacity=%d bytes=%d", cap(second.slotValues), second.logicalBytes())
	}
	if value := second.localCells[0].load(); value.materializedData() != int64(42) || !value.Type.Equal(predeclaredRuntimeTypes["Int"]) {
		t.Fatalf("restored local lost its type or new argument: %#v", value)
	}
	second.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	second.push(newVMValue("String", "live"))
	second.beginSlotInstruction(1, &preparedInstruction{operands: &ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}}})
	value, err := second.pop()
	if err != nil || value.Data != "live" {
		t.Fatalf("restored frame result: %#v, %v", value, err)
	}
	second.recycle()
}

func TestReusableFrameClearsInvocationState(t *testing.T) {
	intType := predeclaredRuntimeTypes["Int"]
	function := loadedFunction{
		Decl: ir.Function{
			ID:     "fn.reusable",
			Code:   &ir.SlotCode{Types: []types.TypeRef{types.Builtin(types.PrimitiveString)}},
			Locals: []ir.Local{{ID: "local.value", Type: types.Builtin(types.PrimitiveInt)}},
		},
		LocalTypes:    []vmType{intType},
		LocalVariadic: []bool{false},
	}
	module := &moduleInstance{framePools: make(map[string][]*frame)}
	first, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(7))}, nil, 1)
	if err != nil || !allocated {
		t.Fatalf("first frame = (%v, %v), want a new frame", allocated, err)
	}
	first.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	first.push(newVMValue("String", "stale"))
	first.recycle()

	second, allocated, err := newFrame(module, function, nil, nil, 2)
	if err != nil || allocated {
		t.Fatalf("second frame = (%v, %v), want a pooled frame", allocated, err)
	}
	if second != first {
		t.Fatal("pooled invocation did not reuse the frame")
	}
	if second.slotValues[0].Data != nil || second.slotOperands != nil || second.localCells[0].initialized {
		t.Fatal("pooled frame retained invocation state")
	}
	value := second.localCells[0].load()
	if value.materializedData() != int64(0) {
		t.Fatalf("reused local = %#v, want zero value", value)
	}
}

func TestFramePoolRejectsFrameAboveModuleBudget(t *testing.T) {
	function := loadedFunction{Decl: ir.Function{ID: "fn.large", Code: &ir.SlotCode{Types: make([]types.TypeRef, maxIdleFrameBytesPerModule/int(ir.RuntimeSlotBytes)+1)}}}
	module := &moduleInstance{framePools: make(map[string][]*frame)}
	callFrame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	callFrame.recycle()
	if len(module.framePools) != 0 || module.framePoolBytes != 0 {
		t.Fatalf("oversized frame entered pool: pools=%d bytes=%d", len(module.framePools), module.framePoolBytes)
	}
}

func TestFrameOperandsRemainRootedUntilReleaseAndReuseResultStorage(t *testing.T) {
	callFrame := &frame{slotValues: make([]vmValue, 2), slotPC: -1}
	callFrame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0, 1}}})
	callFrame.push(newVMValue("String", "first"))
	callFrame.push(newVMValue("String", "second"))
	callFrame.beginSlotInstruction(1, &preparedInstruction{operands: &ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}, {Kind: ir.OperandSlot, Index: 1}}, Release: []uint32{0, 1}}})
	values, err := callFrame.popN(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Data != "first" || values[1].Data != "second" {
		t.Fatalf("popped values = %#v", values)
	}
	if callFrame.slotValues[0].Data != "first" || callFrame.slotValues[1].Data != "second" {
		t.Fatal("inputs were consumed before instruction commit")
	}
	storage := &values[0]
	callFrame.beginSlotInstruction(2, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	if callFrame.slotValues[0].Data != nil || callFrame.slotValues[1].Data != nil {
		t.Fatal("released inputs retained values")
	}
	callFrame.push(newVMValue("String", "third"))
	callFrame.beginSlotInstruction(3, &preparedInstruction{operands: &ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}}})
	values, err = callFrame.popN(1)
	if err != nil {
		t.Fatal(err)
	}
	if &values[0] != storage {
		t.Fatal("popN did not reuse frame-owned result storage")
	}
}

func TestEscapingLocalSurvivesFrameReuse(t *testing.T) {
	intType := predeclaredRuntimeTypes["Int"]
	function := loadedFunction{
		Decl:          ir.Function{ID: "fn.escaping", Locals: []ir.Local{{ID: "local.value", Type: types.Builtin(types.PrimitiveInt)}}, Code: &ir.SlotCode{}},
		LocalIndexes:  map[string]int{"local.value": 0},
		LocalTypes:    []vmType{intType},
		LocalVariadic: []bool{false},
		LocalEscapes:  []bool{true},
	}
	module := &moduleInstance{framePools: make(map[string][]*frame)}
	first, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(7))}, nil, 1)
	if err != nil || !allocated {
		t.Fatalf("frame = (%v, %v), want a new frame", allocated, err)
	}
	escaped, err := first.capture(ir.AddressPayload{Kind: "local", Local: "local.value"})
	if err != nil {
		t.Fatalf("capture local: %v", err)
	}
	first.recycle()
	second, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(9))}, nil, 2)
	if err != nil || allocated || second != first {
		t.Fatalf("second frame = (%v, %v), want the pooled frame", allocated, err)
	}
	if second.localCells[0] == escaped {
		t.Fatal("pooled frame reused an escaping local slot")
	}
	if got := escaped.load().materializedData(); got != int64(7) {
		t.Fatalf("escaped local = %#v, want 7", got)
	}
	if got := second.localCells[0].load().materializedData(); got != int64(9) {
		t.Fatalf("current local = %#v, want 9", got)
	}
}

func TestAddressPathKeepsEvaluatedIndexAfterFrameReuse(t *testing.T) {
	intType := predeclaredRuntimeTypes["Int"]
	arrayType := runtimeTypeFromText("Array<2, Int>")
	function := loadedFunction{
		Decl: ir.Function{
			ID:   "fn.indexed",
			Code: &ir.SlotCode{},
			Locals: []ir.Local{
				{ID: "local.array", Type: arrayType.Ref},
				{ID: "local.index", Type: types.Builtin(types.PrimitiveInt)},
			},
		},
		LocalIndexes:  map[string]int{"local.array": 0, "local.index": 1},
		LocalTypes:    []vmType{arrayType, intType},
		LocalVariadic: []bool{false, false},
		LocalEscapes:  []bool{true, false},
	}
	module := &moduleInstance{framePools: make(map[string][]*frame)}
	array := newVMValue(arrayType, []vmValue{
		newVMValue(intType, int64(1)),
		newVMValue(intType, int64(2)),
	})
	first, _, err := newFrame(module, function, []vmValue{array, newVMValue(intType, int64(0))}, nil, 1)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	pointer, err := first.address(ir.AddressPayload{
		Kind:  "local",
		Local: "local.array",
		Path:  []ir.AddressPathSegment{{Kind: "index", Local: "local.index"}},
	})
	if err != nil {
		t.Fatalf("take indexed address: %v", err)
	}
	escapedArray := first.localCells[0]
	first.recycle()
	if _, _, err := newFrame(module, function, []vmValue{array, newVMValue(intType, int64(1))}, nil, 2); err != nil {
		t.Fatalf("reuse frame: %v", err)
	}
	if err := storePointer(pointer, newVMValue(intType, int64(9))); err != nil {
		t.Fatalf("store through escaped pointer: %v", err)
	}
	values := escapedArray.load().Data.(*vmArray).values()
	if values[0].materializedData() != int64(9) || values[1].materializedData() != int64(2) {
		t.Fatalf("escaped array = %#v, want [9 2]", escapedArray.load().Data)
	}
}

func TestFramePoolBoundsIdleFrames(t *testing.T) {
	function := loadedFunction{Decl: ir.Function{ID: "fn.bounded", Code: &ir.SlotCode{}}}
	module := &moduleInstance{framePools: make(map[string][]*frame)}
	frames := make([]*frame, maxIdleFramesPerFunction+4)
	for i := range frames {
		var err error
		frames[i], _, err = newFrame(module, function, nil, nil, int64(i+1))
		if err != nil {
			t.Fatalf("new frame %d: %v", i, err)
		}
	}
	for _, frame := range frames {
		frame.recycle()
	}
	if got := len(module.framePools[function.Decl.ID]); got != maxIdleFramesPerFunction {
		t.Fatalf("idle frame count = %d, want %d", got, maxIdleFramesPerFunction)
	}
}
