package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestConcurrentAllocationChargesRespectSharedLimit(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		machine := &vm{limits: normalizeLimits(Limits{MaxAllocatedBytes: 1024})}
		machine.liveGuestBytes.Store(256)
		var accepted atomic.Int64
		var group sync.WaitGroup
		for range workers {
			group.Go(func() {
				for range 32 {
					err := machine.chargeAllocationBytes(64)
					if err == nil {
						accepted.Add(1)
					} else {
						var limit ResourceLimitError
						if !errors.As(err, &limit) || limit.Code != "execution.allocation_limit" {
							t.Errorf("unexpected allocation result: %v", err)
						}
					}
				}
			})
		}
		group.Wait()
		if accepted.Load() != 12 || machine.allocatedSinceSweep.Load() != 768 || machine.totalAllocatedBytes.Load() != 768 || machine.peakGuestBytes.Load() != 1024 {
			t.Fatalf("workers=%d accepted=%d live additions=%d total=%d peak=%d", workers, accepted.Load(), machine.allocatedSinceSweep.Load(), machine.totalAllocatedBytes.Load(), machine.peakGuestBytes.Load())
		}
	}
}

func TestAllocationLimitSweepsDiscardedGuestValues(t *testing.T) {
	machine := &vm{limits: normalizeLimits(Limits{MaxAllocatedBytes: 512})}
	machine.owner.Store(true)
	for range 100 {
		if err := machine.chargeAllocationBytes(128); err != nil {
			t.Fatalf("discarded allocation exhausted live budget: %v", err)
		}
	}
	if got := machine.refreshLiveGuestBytes(); got != 0 {
		t.Fatalf("live guest bytes = %d, want 0", got)
	}
	if got := machine.totalAllocatedBytes.Load(); got != 12_800 {
		t.Fatalf("total allocated bytes = %d, want 12800", got)
	}
}

func TestAllocationLimitRejectsRetainedGuestValues(t *testing.T) {
	cell := &slot{initialized: true, value: newVMValue("String", string(make([]byte, 513)))}
	module := &moduleInstance{state: &moduleState{globals: map[string]*slot{"value": cell}}}
	registry := &moduleRegistry{modules: map[string]*moduleInstance{"test": module}}
	machine := &vm{limits: normalizeLimits(Limits{MaxAllocatedBytes: 512})}
	machine.owner.Store(true)
	machine.revision.Store(&instanceRevision{modules: registry})
	machine.allocatedSinceSweep.Store(512)
	err := machine.chargeAllocationBytes(1)
	var limit ResourceLimitError
	if !errors.As(err, &limit) || limit.Code != "execution.allocation_limit" {
		t.Fatalf("retained allocation error = %T %v", err, err)
	}
}

func TestParallelAllocationPressureCensusesAndResumesInstructionOnce(t *testing.T) {
	artifact := ir.NewArtifact("memory/census-resume", "main")
	artifact.Constants = []ir.Constant{
		{ID: "const.left", Type: testType("String"), Value: json.RawMessage(`"a"`)},
		{ID: "const.right", Type: testType("String"), Value: json.RawMessage(`"b"`)},
	}
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() String"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.left"})},
			{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.right"})},
			{Op: string(ir.OpBinary), Payload: testPayload(ir.OperatorPayload{Operator: "+"})},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{ResultCount: 1})},
		},
	}}
	const limit = int64(1 << 20)
	instance, err := patchTestProgram(t, artifact, "census-resume").Instantiate(t.Context(), InstanceOptions{
		Parallelism: 2,
		Limits:      Limits{MaxAllocatedBytes: limit},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	before := instance.vm.totalAllocatedBytes.Load()
	instance.vm.allocatedSinceSweep.Store(limit)
	if state, executed, err := execution.PollSteps(3); err != nil || state != ExecutionRunning || executed != 3 {
		t.Fatalf("pressure slice = %s, %d, %v", state, executed, err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text, ok := result.Values[0].StringValue()
	if !ok || text != "ab" {
		t.Fatalf("result = %#v", result.Values)
	}
	if delta := instance.vm.totalAllocatedBytes.Load() - before; delta != 2 {
		t.Fatalf("retried allocation charged %d bytes, want 2", delta)
	}
}

func TestHostResultDoesNotConsumeGuestBudget(t *testing.T) {
	limits := normalizeLimits(Limits{MaxAllocatedBytes: 1, MaxStringBytes: 1024})
	result, err := hostResult(vmResult{Values: []vmValue{newVMValue("String", "host-owned")}}, limits)
	if err != nil || len(result.Values) != 1 {
		t.Fatalf("host result = %#v, %v", result, err)
	}
	text, ok := result.Values[0].StringValue()
	if !ok || text != "host-owned" {
		t.Fatalf("host result = %#v, %v", result, err)
	}
}

func TestWaitableQueueClearsConsumedSlots(t *testing.T) {
	resource := &waitableResource{Capacity: 2, Buffer: make([]vmValue, 0, 2)}
	resource.appendBuffer(newVMValue("String", "first"))
	resource.appendBuffer(newVMValue("String", "second"))
	if got := resource.popBuffer().Data; got != "first" {
		t.Fatalf("first value = %v", got)
	}
	if resource.Buffer[0].Data != nil {
		t.Fatalf("consumed queue slot retained %#v", resource.Buffer[0])
	}
	resource.appendBuffer(newVMValue("String", "third"))
	if resource.bufferHead != 0 || resource.bufferLen() != 2 {
		t.Fatalf("compacted queue = head %d length %d", resource.bufferHead, resource.bufferLen())
	}
	if got := resource.popBuffer().Data; got != "second" {
		t.Fatalf("second value = %v", got)
	}
	if got := resource.popBuffer().Data; got != "third" {
		t.Fatalf("third value = %v", got)
	}
}

func TestSliceViewsShareLogicalBacking(t *testing.T) {
	root := newSliceValue("Slice<String>", []vmValue{newVMValue("String", "a"), newVMValue("String", "b")})
	header := root.Data.(*vmSlice)
	view := newSliceViewValue("Slice<String>", header, 1, 1, 1)
	sizer := newRuntimeValueSizer()
	sizer.value(root)
	rootBytes := sizer.bytes
	sizer.value(view)
	if added := sizer.bytes - rootBytes; added != 128 {
		t.Fatalf("slice view added %d bytes, want header only", added)
	}
}

func TestArraySlicesShareBackingAndArrayCopiesOwnTheirStorage(t *testing.T) {
	module := &moduleInstance{}
	array := newVMValue("Array<3, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2)), newVMValue("Int", int64(3))})
	first, err := sliceValue(module, array, newVMValue("Int", int64(0)), newVMValue("Int", int64(3)), vmValue{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := sliceValue(module, array, newVMValue("Int", int64(1)), newVMValue("Int", int64(3)), vmValue{})
	if err != nil {
		t.Fatal(err)
	}
	sizer := newRuntimeValueSizer()
	sizer.value(array)
	before := sizer.bytes
	sizer.value(first)
	sizer.value(second)
	if added := sizer.bytes - before; added != 256 {
		t.Fatalf("array views charged backing again: added %d bytes", added)
	}
	copied := module.cloneValueForStore(array)
	if _, err := setIndexValue(module, second, newVMValue("Int", int64(0)), newVMValue("Int", int64(9))); err != nil {
		t.Fatal(err)
	}
	value, err := indexValue(module, first, newVMValue("Int", int64(1)))
	if err != nil || value.materializedData() != int64(9) {
		t.Fatalf("overlapping view lost mutation: %v, %v", value, err)
	}
	value, err = indexValue(module, copied, newVMValue("Int", int64(1)))
	if err != nil || value.materializedData() != int64(2) {
		t.Fatalf("array copy shared mutable backing: %v, %v", value, err)
	}
}

func TestRuntimeValueSizerHandlesDeepGraph(t *testing.T) {
	value := newVMValue("String", "leaf")
	for range 10_000 {
		value = newVMValue("Any", value)
	}
	sizer := newRuntimeValueSizer()
	sizer.value(value)
	if sizer.bytes <= 0 || len(sizer.pendingValues) != 0 || sizer.walking {
		t.Fatalf("deep graph scan did not settle: bytes=%d pending=%d walking=%v", sizer.bytes, len(sizer.pendingValues), sizer.walking)
	}
}

func FuzzRuntimeValueSizerTerminatesOnCycles(f *testing.F) {
	f.Add(uint8(3), "value")
	f.Fuzz(func(t *testing.T, entries uint8, text string) {
		if entries > 32 {
			entries = 32
		}
		valueMap := newVMMap(int(entries))
		root := newVMValue("Map<String,Any>", valueMap)
		for index := uint8(0); index < entries; index++ {
			key := vmMapKey{Kind: vmMapKeyString, Text: string(rune(index))}
			valueMap.storeEntry(key, vmMapEntry{Key: newVMValue("String", key.Text), Value: newVMValue("Any", root)})
		}
		valueMap.storeEntry(vmMapKey{Kind: vmMapKeyString, Text: "text"}, vmMapEntry{Key: newVMValue("String", "text"), Value: newVMValue("String", text)})
		first := newRuntimeValueSizer()
		first.value(root)
		second := newRuntimeValueSizer()
		second.value(root)
		if first.bytes != second.bytes || first.bytes < 0 {
			t.Fatalf("unstable logical size %d/%d", first.bytes, second.bytes)
		}
	})
}
