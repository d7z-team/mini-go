package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"unsafe"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestFrameResidentAdmissionCacheAndRevisionRelease(t *testing.T) {
	function := loadedFunction{Decl: ir.Function{ID: "fn.resident", Code: &ir.SlotCode{}}}
	heap := newManagedHeap()
	heap.maxResidentBytes = uint64(unsafe.Sizeof(frame{})) - 1
	machine := &vm{heap: heap}
	module := &moduleInstance{vm: machine}
	if _, _, err := newFrame(module, function, nil, nil, 1); !errors.Is(err, errManagedLimit) {
		t.Fatalf("frame resident admission: %v", err)
	}
	if stats := heap.stats(); stats.residentPageBytes != 0 || stats.reservedBytes != 0 || stats.totalAllocatedBytes != 0 {
		t.Fatalf("failed construction changed accounting: %+v", stats)
	}
	heap.maxResidentBytes++
	revision := &instanceRevision{owner: machine, moduleOrder: []*moduleInstance{module}}
	machine.revision.Store(revision)
	module.revision = revision
	first, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	physical := heap.stats().residentPageBytes
	first.recycle()
	if heap.stats().residentPageBytes != physical || uint64(machine.idleFrameBytes.Load()) != physical {
		t.Fatal("cached buffers lost resident ownership")
	}
	second, allocated, err := newFrame(module, function, nil, nil, 2)
	if err != nil || allocated || second != first || heap.stats().residentPageBytes != physical {
		t.Fatalf("cached frame double-admitted backing: allocated=%v err=%v", allocated, err)
	}
	second.recycle()
	revision.close()
	if heap.stats().residentPageBytes != 0 || machine.idleFrameBytes.Load() != 0 || len(module.framePools) != 0 {
		t.Fatal("revision close retained frame backing")
	}
}

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

func TestReboundFrameLocalSurvivesTemporaryReuse(t *testing.T) {
	module := newManagedValueTestModule()
	typ := predeclaredRuntimeTypes["String"]
	function := loadedFunction{
		Decl:       ir.Function{ID: "fn.rebind", Locals: []ir.Local{{ID: "value", Type: typ.Ref}}, Code: &ir.SlotCode{Types: []types.TypeRef{typ.Ref}}},
		LocalTypes: []vmType{typ}, LocalVariadic: []bool{false}, LocalEscapes: []bool{false},
	}
	module.executable = &executable{Artifact: ir.NewArtifact("example/module", "main")}
	callFrame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer callFrame.recycle()
	callFrame.storeSlotValue(0, newVMValue(typ, "before call"))
	inst := &preparedInstruction{op: preparedStoreLocal, localIndex: 0, local: &ir.LocalPayload{Local: "value", Rebind: true}, operands: &ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}}}
	callFrame.beginSlotInstruction(0, inst)
	if err := module.vm.executeInstruction(nil, callFrame, inst); err != nil {
		t.Fatal(err)
	}
	callFrame.clearSlotValue(0)
	if got := callFrame.localCells[0].load().payload(); got != "before call" {
		t.Fatalf("rebound local after releasing temporary = %v", got)
	}
	callFrame.storeSlotValue(0, callFrame.localCells[0].load())
	if got := callFrame.slotValue(0).payload(); got != "before call" {
		t.Fatalf("reloaded local after temporary reuse = %v", got)
	}
}

func TestScalarLocalUsesFrameBankWhileEscapingLocalKeepsCellStorage(t *testing.T) {
	intType := predeclaredRuntimeTypes["Int"]
	function := loadedFunction{
		Decl: ir.Function{ID: "fn.frame-bank", Locals: []ir.Local{
			{ID: "scalar", Type: types.Builtin(types.PrimitiveInt)},
			{ID: "escaped", Type: types.Builtin(types.PrimitiveInt)},
		}, Code: &ir.SlotCode{}},
		LocalTypes: []vmType{intType, intType}, LocalVariadic: []bool{false, false},
		LocalEscapes: []bool{false, true},
	}
	module := &moduleInstance{framePools: make(map[string][]*frame)}
	callFrame, _, err := newFrame(module, function, []vmValue{
		newVMValue(intType, int64(11)), newVMValue(intType, int64(22)),
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if callFrame.localCells[0].directOwner != callFrame {
		t.Fatal("scalar local did not use the frame bank")
	}
	if callFrame.localCells[1].directOwner != nil {
		t.Fatal("escaping local used the direct frame bank")
	}
	if got := callFrame.localCells[0].load().materializedData(); got != int64(11) {
		t.Fatalf("direct local = %v, want 11", got)
	}
	if got := callFrame.localCells[1].load().materializedData(); got != int64(22) {
		t.Fatalf("escaping local = %v, want 22", got)
	}
	if callFrame.localStorage[0].initialized || callFrame.localStorage[0].value.Type.Valid() {
		t.Fatal("direct local populated the interface-backed slot storage")
	}
	callFrame.recycle()
}

func TestSmallScalarAggregateLocalUsesInlineFrameBank(t *testing.T) {
	arrayType := runtimeTypeFromText("Array<2, Int>")
	intType := predeclaredRuntimeTypes["Int"]
	function := loadedFunction{
		Decl:       ir.Function{ID: "fn.inline-array", Locals: []ir.Local{{ID: "array", Type: arrayType.Ref}}, Code: &ir.SlotCode{}},
		LocalTypes: []vmType{arrayType}, LocalVariadic: []bool{false}, LocalEscapes: []bool{false},
	}
	module := &moduleInstance{
		vm:         &vm{valueLayouts: newPreparedLayoutRegistry()},
		framePools: make(map[string][]*frame),
	}
	input := newVMValue(arrayType, []vmValue{newVMValue(intType, int64(3)), newVMValue(intType, int64(5))})
	callFrame, _, err := newFrame(module, function, []vmValue{input}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	cell := callFrame.localCells[0]
	if cell.inlineOwner != callFrame || cell.managedCell != nil {
		t.Fatalf("aggregate local storage = inline:%v managed:%v", cell.inlineOwner == callFrame, cell.managedCell != nil)
	}
	loaded, ok := cell.load().payload().(*vmArray)
	if !ok || loaded == nil || loaded.valueAt(0).materializedData() != int64(3) || loaded.valueAt(1).materializedData() != int64(5) {
		t.Fatalf("inline array = %#v", cell.load())
	}
	if err := cell.store(newVMValue(arrayType, []vmValue{newVMValue(intType, int64(7)), newVMValue(intType, int64(11))})); err != nil {
		t.Fatal(err)
	}
	callFrame.recycle()
	reused, _, err := newFrame(module, function, nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	zero, ok := reused.localCells[0].load().payload().(*vmArray)
	if !ok || zero == nil || zero.valueAt(0).materializedData() != int64(0) || zero.valueAt(1).materializedData() != int64(0) {
		t.Fatalf("reused inline array retained data = %#v", reused.localCells[0].load())
	}
	reused.recycle()
}

func TestLogicalInlineFrameRetainsPaddedPhysicalAndManagedLanes(t *testing.T) {
	typeText := "struct{" +
		"B0:Bool,I0:Int64,B1:Bool,I1:Int64,B2:Bool,I2:Int64," +
		"B3:Bool,I3:Int64,B4:Bool,I4:Int64,B5:Bool,I5:Int64," +
		"B6:Bool,I6:Int64,Values:Slice<Int>,Dynamic:Any}"
	typ := runtimeTypeFromText(typeText)
	module := newManagedValueTestModule()
	module.framePools = make(map[string][]*frame)
	layout, err := module.vm.valueLayouts.layoutFor(typ)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Logical > maxInlineAggregateBytes || layout.Stride <= maxInlineAggregateBytes {
		t.Fatalf("padding layout = stride:%d logical:%d, want physical overflow with logical inline", layout.Stride, layout.Logical)
	}
	if len(layout.References) != 1 || len(layout.Dynamics) != 1 {
		t.Fatalf("padding layout lanes = references:%v dynamics:%v", layout.References, layout.Dynamics)
	}
	function := loadedFunction{
		Decl: ir.Function{
			ID:     "fn.padded-inline",
			Locals: []ir.Local{{ID: "value", Type: typ.Ref}},
			Code:   &ir.SlotCode{},
		},
		LocalTypes: []vmType{typ}, LocalVariadic: []bool{false}, LocalEscapes: []bool{false},
	}
	fields := map[string]vmValue{
		"Values":  newOwnedSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(17))}),
		"Dynamic": newVMValue("Any", newVMValue("Int", int64(23))),
	}
	for index := 0; index < 7; index++ {
		fields[fmt.Sprintf("B%d", index)] = newVMValue("Bool", index%2 == 0)
		fields[fmt.Sprintf("I%d", index)] = newVMValue("Int64", int64(index+1))
	}
	input := newRuntimeStructValue(module, typ, fields)
	first, _, err := newFrame(module, function, []vmValue{input}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.localCells[0].inlineOwner != first || first.localCells[0].managedCell != nil {
		t.Fatalf("padded aggregate storage = inline:%v managed:%v", first.localCells[0].inlineOwner == first, first.localCells[0].managedCell != nil)
	}
	wantLogical := ir.StorageFrameHeaderBytes + int64(layout.Logical)
	if got := first.logicalBytes(); got != wantLogical {
		t.Fatalf("frame logical charge = %d, want %d", got, wantLogical)
	}
	loaded := first.localCells[0].load()
	values, err := loadFieldValue(module, loaded, "Values")
	if err != nil || values.payload().(*vmSlice).valueAt(0).materializedData() != int64(17) {
		t.Fatalf("reference lane after inline write = %#v, err=%v", values, err)
	}
	dynamic, err := loadFieldValue(module, loaded, "Dynamic")
	if err != nil {
		t.Fatal(err)
	}
	inner, ok := dynamic.payload().(vmValue)
	if !ok {
		t.Fatalf("dynamic lane after inline write = %#v", dynamic)
	}
	if got, err := asInt64(inner); err != nil || got != 23 {
		t.Fatalf("dynamic lane after inline write = %d, err=%v", got, err)
	}

	second, _, err := newFrame(module, function, nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.localCells[0].store(first.localCells[0].load()); err != nil {
		t.Fatal(err)
	}
	if second.localCells[0].inlineOwner != second {
		t.Fatal("typed aggregate copy left the destination outside its frame bank")
	}
	if err := first.localCells[0].store(newRuntimeStructValue(module, typ, map[string]vmValue{
		"Dynamic": newVMValue("Any", newVMValue("Int", int64(41))),
	})); err != nil {
		t.Fatal(err)
	}
	secondDynamic, err := loadFieldValue(module, second.localCells[0].load(), "Dynamic")
	if err != nil {
		t.Fatal(err)
	}
	secondInner, ok := secondDynamic.payload().(vmValue)
	if !ok {
		t.Fatalf("typed frame copy dynamic lane = %#v", secondDynamic)
	}
	if got, err := asInt64(secondInner); err != nil || got != 23 {
		t.Fatalf("typed frame copy aliases source dynamic lane: %d, err=%v", got, err)
	}
	tempFunction := loadedFunction{
		Decl:           ir.Function{ID: "fn.padded-temp", Code: &ir.SlotCode{Types: []types.TypeRef{typ.Ref}}},
		TemporaryTypes: []vmType{typ}, TemporaryLayouts: []preparedValueLayout{layout},
	}
	tempFrame, _, err := newFrame(module, tempFunction, nil, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	tempFrame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	tempFrame.push(input)
	if tempFrame.slotValues[0].Data != nil {
		t.Fatalf("padded temporary retained detached aggregate: %#v", tempFrame.slotValues[0])
	}
	if tempFrame.tempHeap[0] {
		t.Fatal("logical-inline padded temporary was redirected to managed storage")
	}
	tempValue := tempFrame.slotValue(0)
	tempDynamic, err := loadFieldValue(module, tempValue, "Dynamic")
	if err != nil {
		t.Fatal(err)
	}
	tempInner, ok := tempDynamic.payload().(vmValue)
	if !ok {
		t.Fatalf("padded temporary dynamic lane = %#v", tempDynamic)
	}
	if got, err := asInt64(tempInner); err != nil || got != 23 {
		t.Fatalf("padded temporary dynamic lane = %d, err=%v", got, err)
	}
	tempFrame.recycle()
	first.recycle()
	second.recycle()

	zero, _, err := newFrame(module, function, nil, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	zeroValues, err := loadFieldValue(module, zero.localCells[0].load(), "Values")
	if err != nil {
		t.Fatal(err)
	}
	slice, ok := zeroValues.payload().(*vmSlice)
	if !ok || slice != nil && slice.Len != 0 {
		t.Fatalf("reused padded frame retained reference lane: %#v", zeroValues)
	}
	zeroDynamic, err := loadFieldValue(module, zero.localCells[0].load(), "Dynamic")
	if err != nil {
		t.Fatal(err)
	}
	if zeroDynamic.Type.Ref.Kind != types.Any || zeroDynamic.payload() != nil {
		t.Fatalf("reused padded frame retained dynamic lane: %#v", zeroDynamic)
	}
	zero.recycle()
}

func TestAggregateTemporaryUsesTypedFrameStorage(t *testing.T) {
	arrayType := runtimeTypeFromText("Array<2, Int>")
	function := loadedFunction{Decl: ir.Function{
		ID:   "fn.temp-array",
		Code: &ir.SlotCode{Types: []types.TypeRef{arrayType.Ref}},
	}}
	module := &moduleInstance{
		vm:         &vm{valueLayouts: newPreparedLayoutRegistry()},
		framePools: make(map[string][]*frame),
	}
	artifact := ir.NewArtifact("example/module", "main")
	artifact.TypeTable = *arrayType.Table
	module.executable = &executable{Artifact: artifact}
	callFrame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer callFrame.recycle()
	if callFrame.tempLayouts[0].Stride == 0 {
		t.Fatal("aggregate temporary did not receive a typed layout")
	}
	callFrame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	callFrame.push(newVMValue(arrayType, []vmValue{
		newVMValue("Int", int64(4)), newVMValue("Int", int64(9)),
	}))
	if callFrame.slotValues[0].Data != nil {
		t.Fatalf("temporary retained host payload: %#v", callFrame.slotValues[0])
	}
	value := callFrame.slotValue(0)
	array, ok := value.Data.(*vmArray)
	if !ok || array == nil || array.valueAt(0).materializedData() != int64(4) || array.valueAt(1).materializedData() != int64(9) {
		t.Fatalf("typed temporary = %#v", value)
	}
}

func TestAggregateFrameBankCopyDoesNotAliasSource(t *testing.T) {
	arrayType := runtimeTypeFromText("Array<2, Int>")
	function := loadedFunction{Decl: ir.Function{ID: "fn.bank-copy", Code: &ir.SlotCode{Types: []types.TypeRef{arrayType.Ref, arrayType.Ref}}}}
	module := &moduleInstance{
		vm:         &vm{valueLayouts: newPreparedLayoutRegistry()},
		framePools: make(map[string][]*frame),
	}
	artifact := ir.NewArtifact("example/module", "main")
	artifact.TypeTable = *arrayType.Table
	module.executable = &executable{Artifact: artifact}
	frame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer frame.recycle()
	frame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	frame.push(newVMValue(arrayType, []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2))}))
	frame.beginSlotInstruction(1, &preparedInstruction{operands: &ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}, Outputs: []uint32{1}}})
	frame.push(frame.slotValue(0))
	copied := frame.slotValue(1)
	array, ok := copied.Data.(*vmArray)
	if !ok || array == nil || array.valueAt(0).materializedData() != int64(1) || array.valueAt(1).materializedData() != int64(2) {
		t.Fatalf("copied aggregate = %#v", copied)
	}
	if err := frame.writeTempValue(0, newVMValue(arrayType, []vmValue{newVMValue("Int", int64(7)), newVMValue("Int", int64(8))})); err != nil {
		t.Fatal(err)
	}
	if got := frame.slotValue(1).Data.(*vmArray).valueAt(0).materializedData(); got != int64(1) {
		t.Fatalf("frame bank copy aliases source: got %v", got)
	}
}

func TestLargeAggregateTemporaryUsesManagedReference(t *testing.T) {
	arrayType := runtimeTypeFromText("Array<32, Int>")
	function := loadedFunction{Decl: ir.Function{
		ID:   "fn.large-temp-array",
		Code: &ir.SlotCode{Types: []types.TypeRef{arrayType.Ref}},
	}}
	module := newManagedValueTestModule()
	module.executable = &executable{Artifact: ir.Artifact{TypeTable: *arrayType.Table}}
	frame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer frame.recycle()
	if !frame.tempHeap[0] || frame.tempStorage != nil && len(frame.tempStorage) != 0 {
		t.Fatalf("large temporary storage = heap:%v raw:%d", frame.tempHeap[0], len(frame.tempStorage))
	}
	items := make([]vmValue, 32)
	for index := range items {
		items[index] = newVMValue("Int", int64(index+1))
	}
	frame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	frame.push(newVMValue(arrayType, items))
	if !frame.slotValues[0].ref.valid() || frame.slotValues[0].Data != nil {
		t.Fatalf("large temporary retained host payload: %#v", frame.slotValues[0])
	}
	value := frame.slotValue(0)
	array, ok := value.payload().(*vmArray)
	if !ok || array == nil || array.valueAt(0).materializedData() != int64(1) || array.valueAt(31).materializedData() != int64(32) {
		t.Fatalf("large typed temporary = %#v", value)
	}
	frame.clearSlotValue(0)
	if frame.tempInitialized[0] || frame.slotValues[0].ref.valid() {
		t.Fatalf("cleared large temporary retained value: %#v", frame.slotValues[0])
	}
}

func TestDynamicLocalUsesPreparedDescriptorStorage(t *testing.T) {
	typ := predeclaredRuntimeTypes["Any"]
	function := loadedFunction{
		Decl:       ir.Function{ID: "fn.dynamic-local", Locals: []ir.Local{{ID: "value", Type: typ.Ref}}, Code: &ir.SlotCode{}},
		LocalTypes: []vmType{typ}, LocalVariadic: []bool{false}, LocalEscapes: []bool{false},
	}
	module := newManagedValueTestModule()
	frame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer frame.recycle()
	if frame.localCells[0].inlineOwner != frame {
		t.Fatal("dynamic local did not use prepared frame storage")
	}
	inner := newVMValue("Int", int64(37))
	if err := frame.localCells[0].store(newVMValue(typ, inner)); err != nil {
		t.Fatal(err)
	}
	value := frame.localCells[0].load()
	decoded, ok := value.payload().(vmValue)
	if !ok || decoded.materializedData() != int64(37) {
		t.Fatalf("dynamic local = %#v", value)
	}
}

func TestScalarTemporaryUsesPreparedLane(t *testing.T) {
	function := loadedFunction{Decl: ir.Function{
		ID:   "fn.temp-int",
		Code: &ir.SlotCode{Types: []types.TypeRef{types.Builtin(types.PrimitiveInt)}},
	}}
	module := &moduleInstance{vm: &vm{valueLayouts: newPreparedLayoutRegistry()}, framePools: make(map[string][]*frame)}
	frame, _, err := newFrame(module, function, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer frame.recycle()
	if frame.tempLayouts[0].Stride == 0 {
		t.Fatal("scalar temporary did not use a prepared lane")
	}
	frame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{0}}})
	frame.push(newVMValue("Int", int64(41)))
	if frame.slotValues[0].Data != nil {
		t.Fatalf("scalar temporary retained host payload: %#v", frame.slotValues[0])
	}
	if got := frame.slotValue(0).materializedData(); got != int64(41) {
		t.Fatalf("scalar temporary = %#v", got)
	}
}

func TestComplexFrameLocalPreservesBothComponents(t *testing.T) {
	for _, name := range []string{"Complex64", "Complex128"} {
		t.Run(name, func(t *testing.T) {
			typ := runtimeTypeFromText(name)
			function := loadedFunction{
				Decl:       ir.Function{ID: "fn.complex", Locals: []ir.Local{{ID: "value", Type: typ.Ref}}, Code: &ir.SlotCode{}},
				LocalTypes: []vmType{typ}, LocalVariadic: []bool{false}, LocalEscapes: []bool{false},
			}
			module := newManagedValueTestModule()
			callFrame, _, err := newFrame(module, function, []vmValue{newVMValue(typ, complex(1.5, -2.25))}, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer callFrame.recycle()
			value := callFrame.localCells[0].load()
			if got, ok := value.payload().(complex128); !ok || got != complex(1.5, -2.25) {
				t.Fatalf("complex local = %v (%T)", value.payload(), value.payload())
			}
			if err := callFrame.localCells[0].store(newVMValue(typ, complex(-3.5, 9.25))); err != nil {
				t.Fatal(err)
			}
			if got := callFrame.localCells[0].load().payload(); got != complex(-3.5, 9.25) {
				t.Fatalf("complex assignment = %v", got)
			}
		})
	}
}

func TestInlineAggregateReferenceLanesUseManagedHandles(t *testing.T) {
	arrayType := runtimeTypeFromText("Array<2, String>")
	function := loadedFunction{
		Decl:       ir.Function{ID: "fn.inline-string-array", Locals: []ir.Local{{ID: "array", Type: arrayType.Ref}}, Code: &ir.SlotCode{}},
		LocalTypes: []vmType{arrayType}, LocalVariadic: []bool{false}, LocalEscapes: []bool{false},
	}
	module := &moduleInstance{
		vm:         &vm{heap: newManagedHeap(), valueLayouts: newPreparedLayoutRegistry()},
		framePools: make(map[string][]*frame),
	}
	input := newVMValue(arrayType, []vmValue{newVMValue("String", "left"), newVMValue("String", "right")})
	callFrame, _, err := newFrame(module, function, []vmValue{input}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	cell := callFrame.localCells[0]
	if cell.inlineOwner != callFrame || cell.managedCell != nil {
		t.Fatalf("aggregate reference storage = inline:%v managed:%v", cell.inlineOwner == callFrame, cell.managedCell != nil)
	}
	layout, raw, ok := callFrame.inlineLayout(0)
	if !ok || len(layout.References) != 2 {
		t.Fatalf("inline reference layout = %#v, raw=%d, ok=%v", layout, len(raw), ok)
	}
	for _, offset := range layout.References {
		if ref := (guestRef{slot: binary.LittleEndian.Uint32(raw[offset:]), generation: binary.LittleEndian.Uint32(raw[offset+4:])}); !ref.valid() {
			t.Fatalf("inline reference at %d is not a managed handle", offset)
		}
	}
	machine := module.vm
	machine.machine = &executionMachine{vm: machine, tasks: map[int64]*executionTask{1: {id: 1, frames: []*executionFrame{{frame: callFrame}}}}}
	var roots []guestRef
	machine.visitManagedRoots(func(ref guestRef) { roots = append(roots, ref) })
	for _, offset := range layout.References {
		want := guestRef{slot: binary.LittleEndian.Uint32(raw[offset:]), generation: binary.LittleEndian.Uint32(raw[offset+4:])}
		found := false
		for _, root := range roots {
			if root == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("inline reference %v was not enumerated as a frame root", want)
		}
	}
	loaded, ok := cell.load().payload().(*vmArray)
	if !ok || loaded == nil || loaded.valueAt(0).materializedData() != "left" || loaded.valueAt(1).materializedData() != "right" {
		t.Fatalf("inline string array = %#v", cell.load())
	}
	callFrame.recycle()
}

func TestOversizedFrameReleasesStorageAndRebuildsInvocation(t *testing.T) {
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
	if module.framePoolBytes != 0 || module.vm.idleFrameBytes.Load() != 0 {
		t.Fatalf("guest/physical pool accounting: %d/%d", module.framePoolBytes, module.vm.idleFrameBytes.Load())
	}
	if first.slotValues != nil {
		t.Fatal("oversized physical backing retained")
	}
	second, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(42))}, nil, 2)
	if err != nil || !allocated {
		t.Fatalf("new activation: allocated=%v err=%v", allocated, err)
	}
	if cap(second.slotValues) != capacity || second.logicalBytes() != logical {
		t.Fatalf("new activation layout: capacity=%d bytes=%d", cap(second.slotValues), second.logicalBytes())
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
	if second.localCells[0].directOwner != second || second.localCells[0].directIndex != 0 {
		t.Fatalf("pooled local owner = %p/%d, want %p/0", second.localCells[0].directOwner, second.localCells[0].directIndex, second)
	}
	if second.slotValues[0].Data != nil || second.slotOperands != nil || second.localCells[0].initialized {
		t.Fatal("pooled frame retained invocation state")
	}
	value := second.localCells[0].load()
	if value.materializedData() != int64(0) {
		t.Fatalf("reused local = %#v, want zero value", value)
	}
}

func TestFramePoolDropsIdleFrameAcrossRevisionPatch(t *testing.T) {
	oldRevision := &instanceRevision{generation: 1}
	newRevision := &instanceRevision{generation: 2}
	machine := &vm{}
	machine.revision.Store(oldRevision)
	module := &moduleInstance{vm: machine, revision: oldRevision, framePools: make(map[string][]*frame)}
	function := loadedFunction{
		Decl:       ir.Function{ID: "fn.patch-shape", Code: &ir.SlotCode{}},
		LocalTypes: []vmType{predeclaredRuntimeTypes["Int"]}, LocalVariadic: []bool{false},
	}
	first, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(1))}, nil, 1)
	if err != nil || !allocated {
		t.Fatalf("initial frame: allocated=%v err=%v", allocated, err)
	}
	first.recycle()
	if len(module.framePools[function.Decl.ID]) != 1 {
		t.Fatalf("initial frame was not cached: %+v", module.framePools)
	}
	machine.revision.Store(newRevision)
	module.revision = newRevision
	second, allocated, err := newFrame(module, function, []vmValue{newVMValue("Int", int64(2))}, nil, 2)
	if err != nil || !allocated || second == first {
		t.Fatalf("patched frame reused stale activation: allocated=%v same=%v err=%v", allocated, second == first, err)
	}
	if idle := len(module.framePools[function.Decl.ID]); idle != 0 {
		t.Fatalf("discarded revision left %d idle frame entries", idle)
	}
	third, allocated, err := newFrame(module, function, nil, nil, 3)
	if err != nil || !allocated || third == first || third == second {
		t.Fatalf("concurrent patched frame: allocated=%v err=%v", allocated, err)
	}
	third.recycle()
	second.recycle()
}

func TestPreparedLocalForwardCopiesInlineAggregateBytes(t *testing.T) {
	module := newManagedValueTestModule()
	arrayType := runtimeTypeFromText("Array<2, Int>")
	function := loadedFunction{
		Decl: ir.Function{
			ID:   "fn.forward",
			Code: &ir.SlotCode{},
			Locals: []ir.Local{
				{ID: "source", Type: arrayType.Ref},
				{ID: "destination", Type: arrayType.Ref},
			},
		},
		LocalTypes:    []vmType{arrayType, arrayType},
		LocalVariadic: []bool{false, false},
	}
	value := newVMValue(arrayType, []vmValue{newVMValue("Int", int64(7)), newVMValue("Int", int64(42))})
	frame, _, err := newFrame(module, function, []vmValue{value}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer frame.recycle()
	frame.beginSlotInstruction(0, &preparedInstruction{operands: &ir.SlotOperands{Outputs: []uint32{ir.LocalOutput | 1}}})
	copied, err := frame.copyLocalToOutput(0)
	if err != nil || !copied {
		t.Fatalf("prepared local forwarding: copied=%v err=%v", copied, err)
	}
	got := frame.localCells[1].load()
	items, ok := got.Data.(*vmArray)
	if !ok || items.valueAt(0).materializedData() != int64(7) || items.valueAt(1).materializedData() != int64(42) {
		t.Fatalf("forwarded aggregate = %#v", got)
	}
}

func TestInlineCopyRejectsAggregateSubview(t *testing.T) {
	layout := preparedValueLayout{ID: 1, Stride: 8, Logical: 8}
	owner := &frame{
		inlineLayouts:     []preparedValueLayout{layout},
		inlineOffsets:     []uint32{0},
		inlineStorage:     make([]byte, 16),
		inlineInitialized: []bool{true},
	}
	root := vmValue{Data: &vmArray{vmSlice: vmSlice{inlineRaw: owner.inlineStorage[:8]}}, inlineOwner: owner, inlineIndex: 0}
	view := root
	view.Data = &vmArray{vmSlice: vmSlice{inlineRaw: owner.inlineStorage[1:9]}}
	if owner.copyInlineValue(make([]byte, layout.Stride), layout, view) {
		t.Fatal("aggregate subview was copied as the complete source slot")
	}
}

func TestFramePoolRejectsFrameAboveModuleBudget(t *testing.T) {
	function := loadedFunction{Decl: ir.Function{ID: "fn.large", Code: &ir.SlotCode{Types: make([]types.TypeRef, maxIdleFrameBytesPerModule/int(ir.StorageReferenceBytes)+1)}}}
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
