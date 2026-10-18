package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestOwnerReclaimsYoungChurnBeforeScanningStableOldGraph(t *testing.T) {
	module := newManagedValueTestModule()
	heap := module.vm.heap
	t.Cleanup(heap.close)
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	root := managedTestObject(t, heap, layout, 1, 8)
	last := root
	for range 512 {
		next := managedTestObject(t, heap, layout, 1, 8)
		managedTestReference(t, heap, last, 0, next)
		last = next
	}
	module.vm.controlRoots = []vmValue{{ref: root, heap: heap}}
	for range 2 {
		if err := module.vm.collectMinorManaged(); err != nil {
			t.Fatal(err)
		}
	}
	before := heap.stats()
	heap.nextMajorBytes = before.occupiedBytes + 64
	for range 32 {
		managedTestObject(t, heap, layout, 1, 8)
	}
	if !heap.collectionRequested.Load() {
		t.Fatal("allocation growth did not request collection")
	}
	if err := module.vm.collectManagedAtSafepoint(); err != nil {
		t.Fatal(err)
	}
	after := heap.stats()
	if after.occupiedBytes != before.occupiedBytes || after.minorCollections != before.minorCollections+1 || after.majorCollections != before.majorCollections {
		t.Fatalf("young churn should be reclaimed by minor: before=%+v after=%+v", before, after)
	}
	if after.scannedReferences != before.scannedReferences {
		t.Fatalf("young churn rescanned stable old graph: %d edges", after.scannedReferences-before.scannedReferences)
	}
}

func TestOwnerMajorCollectsOldGrowthInOnePass(t *testing.T) {
	module := newManagedValueTestModule()
	heap := module.vm.heap
	t.Cleanup(heap.close)
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	managedTestObject(t, heap, layout, managedPageBytes/8, uint64(managedPageBytes))
	batch, err := heap.acquireBatch(64, 2)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	reservation.commit()
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	heap.nextMajorBytes = uint64(managedPageBytes)
	heap.collectionRequested.Store(true)
	if err := module.vm.collectManagedAtSafepoint(); err != nil {
		t.Fatal(err)
	}
	stats := heap.stats()
	if stats.occupiedBytes != 0 || stats.majorCollections != 1 || stats.minorCollections != 0 {
		t.Fatalf("old growth should collect both generations in one major: %+v", stats)
	}
	if heap.youngBytes != 0 {
		t.Fatalf("major retained young allocation pressure: %d", heap.youngBytes)
	}
}

func TestCallFrameCanUseUnusedWorkerAllocationCredit(t *testing.T) {
	for _, name := range []string{"memory_call", "memory_tail_call"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("../testdata/runtime/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var artifact ir.Artifact
			if err := json.Unmarshal(data, &artifact); err != nil {
				t.Fatal(err)
			}
			artifact.Functions[0].ID = "fn.entry"
			artifact.Format, artifact.Version, artifact.OpcodeSet = ir.Format, ir.CurrentVersion, ir.OpcodeSet
			instance, err := patchTestProgram(t, artifact, name).Instantiate(t.Context(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestInstance(t, instance)
			instance.supervisor.Load().stop()
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			// Enough storage for the call, exactly at the next TLAB admission
			// boundary. Its unused credit must remain usable by frame owners.
			limit := instance.vm.heap.stats().occupiedBytes + managedBatchCredit
			instance.vm.heap.maxGuestBytes = limit
			instance.vm.limits.MaxGuestBytes = int64(limit)
			for {
				state, _, err := execution.PollSteps(64)
				if err != nil {
					t.Fatal(err)
				}
				if state == ExecutionCompleted {
					break
				}
			}
			result, err := execution.Result()
			if err != nil || len(result.Values) != 1 {
				t.Fatalf("call result: %v, %v", result, err)
			}
			if got, ok := result.Values[0].Int64(); !ok || got != 0 {
				t.Fatalf("call result = %v", result.Values)
			}
			if stats := instance.vm.heap.stats(); stats.reservedBytes != 0 || stats.occupiedBytes > limit {
				t.Fatalf("settled accounting = %+v", stats)
			}
		})
	}
}

func TestOwnerCollectionPreservesStorageUsedByNextAssignment(t *testing.T) {
	artifact := ir.NewArtifact("memory/prepared-cell", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Int"),
		Locals: []ir.Local{{ID: "value", Type: testType("Array<32, Int>")}},
		Code: testMicroCode([]string{"Array<32, Int>", "Array<32, Int>", "Int"}, []ir.Instruction{
			{Op: ir.OpZero, Payload: testTypePayload("Array<32, Int>")},
			{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "value", Rebind: true}},
			{Op: ir.OpLoadLocal, Payload: ir.LocalPayload{Local: "value"}},
			{Op: ir.OpLen},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, {2}}, {{2}, nil}}),
	}}
	instance, err := patchTestProgram(t, artifact, "prepared-cell").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := execution.PollSteps(1); err != nil {
		t.Fatal(err)
	}
	instance.vm.heap.poisonCollected = true
	instance.vm.heap.collectionRequested.Store(true)
	result, err := execution.Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := result.Values[0].Int64(); !ok || got != 32 {
		t.Fatalf("post-collection array length = %v", result.Values)
	}
	if instance.vm.heap.stats().minorCollections == 0 {
		t.Fatal("owner did not service the requested collection")
	}
}

func TestOwnerCollectsLargeGarbageWithoutMemoryLimit(t *testing.T) {
	artifact := ir.NewArtifact("memory/large-churn", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Int"),
		Code: testMicroCode([]string{"Array<65536, Int>", "Int"}, []ir.Instruction{
			{Op: ir.OpZero, Payload: testTypePayload("Array<65536, Int>")},
			{Op: ir.OpLen},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}}),
	}}
	instance, err := patchTestProgram(t, artifact, "large-churn").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	for range 24 {
		result, err := instance.Call(t.Context(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if length, ok := result.Values[0].Int64(); !ok || length != 65536 {
			t.Fatalf("large array length = %v", result.Values)
		}
	}
	stats := instance.vm.heap.stats()
	if stats.majorCollections == 0 || stats.reclaimedBytes == 0 {
		t.Fatalf("unbounded execution failed to reclaim large garbage: %+v", stats)
	}
	if stats.occupiedBytes >= stats.totalAllocatedBytes || stats.reservedBytes != 0 {
		t.Fatalf("large allocation ownership did not settle: %+v", stats)
	}
}

func TestConcurrentAllocationChargesRespectSharedLimit(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		machine := &vm{limits: normalizeLimits(Limits{MaxGuestBytes: 1024})}
		machine.heap = newManagedHeap()
		machine.heap.maxGuestBytes = 1024
		if err := machine.chargeAllocationBytes(256); err != nil {
			t.Fatal(err)
		}
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
		stats := machine.heap.stats()
		if accepted.Load() != 12 || stats.occupiedBytes != 1024 || stats.totalAllocatedBytes != 1024 {
			t.Fatalf("workers=%d accepted=%d occupied=%d total=%d", workers, accepted.Load(), stats.occupiedBytes, stats.totalAllocatedBytes)
		}
		if got := stats.reservedBytes; got != 0 {
			t.Fatalf("workers=%d leaked reservation bytes: %d", workers, got)
		}
		if got := stats.occupiedBytes; got != 1024 {
			t.Fatalf("workers=%d occupied bytes = %d, want 1024", workers, got)
		}
	}
}

func TestAllocationLimitRejectsRetainedGuestValues(t *testing.T) {
	machine := &vm{limits: normalizeLimits(Limits{MaxGuestBytes: 512}), heap: newManagedHeap(), valueLayouts: newPreparedLayoutRegistry()}
	module := &moduleInstance{vm: machine, state: &moduleState{globals: map[string]*slot{}}}
	cell := &slot{module: module}
	cell.value = module.adoptValue(newVMValue("String", string(make([]byte, 513))))
	cell.initialized = true
	module.state.globals["value"] = cell
	occupied := machine.heap.stats().occupiedBytes
	if occupied == 0 {
		t.Fatal("retained string did not publish managed storage")
	}
	machine.heap.maxGuestBytes = occupied
	registry := &moduleRegistry{modules: map[string]*moduleInstance{"test": module}}
	machine.owner.Store(true)
	machine.revision.Store(&instanceRevision{modules: registry})
	err := machine.chargeAllocationBytes(1)
	var limit ResourceLimitError
	if !errors.As(err, &limit) || limit.Code != "execution.allocation_limit" {
		t.Fatalf("retained allocation error = %T %v", err, err)
	}
}

func TestManagedLedgerSharesControlAndPayloadBudget(t *testing.T) {
	heap := newManagedHeap()
	heap.maxGuestBytes = 64
	machine := &vm{heap: heap, limits: normalizeLimits(Limits{MaxGuestBytes: 64})}
	layout, err := heap.registerLayout(managedLayout{stride: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer heap.releaseLayout(layout)
	if err := machine.chargeAllocationBytes(32); err != nil {
		t.Fatal("control admission failed:", err)
	}
	reservation, err := heap.reserve(layout, 1, 32)
	if err != nil {
		t.Fatal("payload admission failed beside control record:", err)
	}
	if ref := reservation.commit(); !ref.valid() {
		t.Fatal("payload publication returned an invalid reference")
	}
	if err := machine.chargeAllocationBytes(1); err == nil {
		t.Fatal("control allocation exceeded the shared guest budget")
	}
	machine.releaseAllocationBytes(32)
	if err := machine.chargeAllocationBytes(1); err != nil {
		t.Fatal("released control storage did not return to the shared budget:", err)
	}
	machine.releaseAllocationBytes(1)
	if got := heap.stats().occupiedBytes; got != 32 {
		t.Fatalf("shared ledger occupied bytes = %d, want payload 32", got)
	}
}

func TestFailedFrameChargeCannotBeReusedWithoutAccounting(t *testing.T) {
	artifact := ir.NewArtifact("memory/frame-charge", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{}, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}}),
	}}
	instance, err := patchTestProgram(t, artifact, "frame-charge").Instantiate(t.Context(), InstanceOptions{
		Limits: Limits{MaxGuestBytes: ir.StorageControlRecordBytes + ir.StorageFrameHeaderBytes - 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	initial, err := instance.RuntimeStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := instance.Start("run"); err == nil {
			t.Fatalf("start %d reused an uncharged frame", attempt)
		}
		stats, err := instance.RuntimeStats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.TotalGuestAllocatedBytes != initial.TotalGuestAllocatedBytes || stats.ReservedGuestBytes != initial.ReservedGuestBytes {
			t.Fatalf("start %d charged %d bytes", attempt, stats.TotalGuestAllocatedBytes)
		}
	}
}

func TestTaskAndFrameAdmissionIsAtomic(t *testing.T) {
	artifact := ir.NewArtifact("memory/task-frame-charge", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Code: testMicroCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}}),
	}}
	instance, err := patchTestProgram(t, artifact, "task-frame-charge").Instantiate(t.Context(), InstanceOptions{
		Limits: Limits{MaxGuestBytes: ir.StorageControlRecordBytes + ir.StorageFrameHeaderBytes - 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	initial := instance.vm.heap.stats()
	for range 3 {
		if _, err := instance.Start("run"); err == nil {
			t.Fatalf("task and frame together exceed the configured allocation budget: allocated=%d", instance.vm.heap.stats().totalAllocatedBytes)
		}
		if allocated := instance.vm.heap.stats().totalAllocatedBytes; allocated != initial.totalAllocatedBytes {
			t.Fatalf("failed task construction committed a partial allocation: %d", allocated)
		}
	}
	want := uint64(ir.StorageControlRecordBytes + ir.StorageFrameHeaderBytes)
	instance.vm.limits.MaxGuestBytes = int64(initial.occupiedBytes + want)
	instance.vm.heap.maxGuestBytes = uint64(instance.vm.limits.MaxGuestBytes)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if allocated := instance.vm.heap.stats().totalAllocatedBytes - initial.totalAllocatedBytes; allocated != want {
		t.Fatalf("successful task construction charged %d bytes, want %d", allocated, want)
	}
}

func TestDeferAdmissionFailureLeavesNoPublishedControlRecord(t *testing.T) {
	artifact := ir.NewArtifact("memory/defer-admission", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"function() Void"}, []ir.Instruction{
			{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "fn.cleanup"}},
			{Op: ir.OpDeferPush, Payload: ir.DeferPayload{}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}),
	}, {
		ID: "fn.cleanup", Signature: testSignature("function() Void"),
		Code: testMicroCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}}),
	}}
	program := patchTestProgram(t, artifact, "defer-admission")
	for _, budget := range []string{"guest", "resident"} {
		t.Run(budget, func(t *testing.T) {
			instance, err := program.Instantiate(t.Context(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = instance.Close() })
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := execution.PollSteps(1); err != nil {
				t.Fatal(err)
			}
			before := instance.vm.heap.stats()
			if budget == "guest" {
				instance.vm.limits.MaxGuestBytes = int64(before.occupiedBytes)
				instance.vm.heap.maxGuestBytes = before.occupiedBytes
			} else {
				instance.vm.heap.maxResidentBytes = before.residentPageBytes
			}
			if _, err := execution.Wait(t.Context()); err == nil {
				t.Fatal("defer publication exceeded its budget")
			}
			after := instance.vm.heap.stats()
			if after.reservedBytes != 0 || after.totalAllocatedBytes != before.totalAllocatedBytes {
				t.Fatalf("failed defer committed storage: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestHostResultDoesNotConsumeGuestBudget(t *testing.T) {
	limits := normalizeLimits(Limits{MaxGuestBytes: 1, MaxBoundaryBytes: 1024})
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
	first, err := resource.popBuffer()
	if err != nil || first.Data != "first" {
		t.Fatalf("first value = %v, err=%v", first.Data, err)
	}
	if resource.Buffer[0].Data != nil {
		t.Fatalf("consumed queue slot retained %#v", resource.Buffer[0])
	}
	resource.appendBuffer(newVMValue("String", "third"))
	if resource.bufferHead != 0 || resource.bufferLen() != 2 {
		t.Fatalf("compacted queue = head %d length %d", resource.bufferHead, resource.bufferLen())
	}
	second, err := resource.popBuffer()
	if err != nil || second.Data != "second" {
		t.Fatalf("second value = %v, err=%v", second.Data, err)
	}
	third, err := resource.popBuffer()
	if err != nil || third.Data != "third" {
		t.Fatalf("third value = %v, err=%v", third.Data, err)
	}
}

func TestSliceViewsShareLogicalBacking(t *testing.T) {
	root := newSliceValue("Slice<String>", []vmValue{newVMValue("String", "a"), newVMValue("String", "b")})
	header := root.Data.(*vmSlice)
	view := newSliceViewValue("Slice<String>", header, 1, 1, 1)
	if view.Data == nil || view.Data.(*vmSlice).vmSliceStorage != header.vmSliceStorage {
		t.Fatal("slice view did not retain backing identity")
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
