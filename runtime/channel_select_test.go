package runtime

import (
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestChannelSelectionReservesBeforeRegistration(t *testing.T) {
	const selectionBytes = ir.StorageControlRecordBytes + ir.StorageControlRecordBytes + 3*ir.StorageDynamicBytes
	for _, limit := range []int64{selectionBytes - 1, selectionBytes} {
		vm := &vm{limits: normalizeLimits(Limits{MaxGuestBytes: limit})}
		// This test exercises the reservation transaction itself. A collection
		// safepoint may retire admission debt before a retry, so keep the VM
		// outside the owner context to make the limit deterministic.
		task := &executionTask{id: 1}
		vm.machine = &executionMachine{vm: vm, tasks: map[int64]*executionTask{1: task}}
		module := &moduleInstance{vm: vm}
		resource := &waitableResource{Type: coerceRuntimeType("Waitable<Int>")}
		channel := newVMValue(resource.Type, resource)
		selection, err := vm.prepareChannelSelection(task, module, []channelSelectCase{{channel: channel}})
		if limit < selectionBytes {
			var resourceLimit ResourceLimitError
			if !errors.As(err, &resourceLimit) {
				t.Fatalf("reserve: %v", err)
			}
			if resource.selectHead != nil || resource.selectTail != nil {
				t.Fatal("failed reservation registered a case")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			selection.register()
			vm.machine.cancelBlockedOperation(&executionTask{blocked: &blockedOperation{kind: "select", selection: selection}})
			if resource.selectHead != nil || resource.selectTail != nil {
				t.Fatal("cancellation retained registrations")
			}
		}
	}
}

func TestChannelSelectionCommitsPeersAndWithdrawsOtherCases(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		vm := &vm{limits: normalizeLimits(Limits{})}
		module := &moduleInstance{vm: vm}
		channels := make([]vmValue, 2)
		for index := range channels {
			var err error
			channels[index], err = makeWaitableValue(module, "Waitable<Int>", newVMValue("Int", int64(0)))
			if err != nil {
				t.Fatal(err)
			}
		}
		first, err := vm.prepareChannelSelection(nil, module, []channelSelectCase{
			{channel: channels[0], send: true, value: newVMValue("Int", int64(11))},
			{channel: channels[1]},
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.tryCommit() {
			t.Fatal("unmatched selection committed")
		}
		first.register()
		second, err := vm.prepareChannelSelection(nil, module, []channelSelectCase{
			{channel: channels[1], send: true, value: newVMValue("Int", int64(22))},
			{channel: channels[0]},
		})
		if err != nil {
			t.Fatal(err)
		}
		if cancelFirst {
			first.unregister()
			if second.tryCommit() {
				t.Fatal("communication matched a canceled peer")
			}
			continue
		}
		if !second.tryCommit() || !first.done || first.index == second.index {
			t.Fatal("rendezvous did not select exactly one complementary pair")
		}
		if first.index == 0 {
			got, err := asInt64(second.value)
			if !second.ok || err != nil || got != 11 {
				t.Fatal("first send was not delivered")
			}
		} else {
			got, err := asInt64(first.value)
			if !first.ok || err != nil || got != 22 {
				t.Fatal("second send was not delivered")
			}
		}
		first.unregister()
		second.unregister()
		for _, channel := range channels {
			resource, ok := materializeManagedWaitable(channel)
			if !ok {
				t.Fatal("managed channel did not materialize")
			}
			if resource.selectHead != nil || resource.selectTail != nil || resource.bufferLen() != 0 {
				t.Fatal("committed selection retained losing cases or retransmittable values")
			}
		}
	}
}

func TestChannelSelectionCannotRendezvousWithItself(t *testing.T) {
	vm := &vm{limits: normalizeLimits(Limits{})}
	module := &moduleInstance{vm: vm}
	channel, err := makeWaitableValue(module, "Waitable<Int>", newVMValue("Int", int64(0)))
	if err != nil {
		t.Fatal(err)
	}
	selection, err := vm.prepareChannelSelection(nil, module, []channelSelectCase{
		{channel: channel, send: true, value: newVMValue("Int", int64(42))}, {channel: channel},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection.register()
	if selection.tryCommit() {
		t.Fatal("select paired its own send and receive")
	}
	value, ok, _, err := waitableTryRecvValue(module, channel)
	got, valueErr := asInt64(value)
	if err != nil || !ok || valueErr != nil || got != 42 || !selection.done || selection.index != 0 {
		t.Fatalf("external receive: %v %t %v", value, ok, err)
	}
	resource, ok := materializeManagedWaitable(channel)
	if !ok || resource.selectHead != nil {
		t.Fatal("losing receive remained registered")
	}
}

func TestChannelSelectionReceiveAdmissionFailureIsRetryable(t *testing.T) {
	module := newManagedValueTestModule()
	channel, err := makeWaitableValue(module, "Waitable<Array<2, Int>>", newVMValue("Int", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	resource, err := waitableValueData(module, channel)
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.appendBuffer(newVMValue("Array<2, Int>", []vmValue{
		newVMValue("Int", int64(7)), newVMValue("Int", int64(8)),
	})); err != nil {
		t.Fatal(err)
	}
	selection, err := module.vm.prepareChannelSelection(nil, module, []channelSelectCase{{channel: channel}})
	if err != nil {
		t.Fatal(err)
	}
	// Receiving an aggregate creates independent storage before the ring head
	// advances. Exhausting the guest ledger must therefore leave the value
	// available for the scheduler's same-PC retry.
	module.vm.heap.maxGuestBytes = module.vm.heap.stats().occupiedBytes
	if !selection.tryCommit() {
		t.Fatal("ready receive did not attempt its selected case")
	}
	if !errors.Is(selection.err, errManagedCollection) {
		t.Fatalf("selection error = %v, want managed collection pressure", selection.err)
	}
	if resource.bufferLen() != 1 || !selection.done {
		t.Fatalf("failed receive consumed selection input: len=%d done=%t", resource.bufferLen(), selection.done)
	}

	module.vm.heap.maxGuestBytes = 0
	retry, err := module.vm.prepareChannelSelection(nil, module, []channelSelectCase{{channel: channel}})
	if err != nil {
		t.Fatal(err)
	}
	if !retry.tryCommit() || retry.err != nil {
		t.Fatalf("retry selection = done:%t err:%v", retry.done, retry.err)
	}
	if resource.bufferLen() != 0 {
		t.Fatal("successful retry did not consume the buffered value exactly once")
	}
	array, arrayOK := retry.value.payload().(*vmArray)
	if !arrayOK || !retry.ok || array.valueAt(0).materializedData() != int64(7) {
		t.Fatalf("retry value = %v, ok=%t", retry.value, retry.ok)
	}
}

func TestChannelSelectionCloseSettlesPendingSendAndReceive(t *testing.T) {
	for _, sending := range []bool{false, true} {
		vm := &vm{limits: normalizeLimits(Limits{})}
		module := &moduleInstance{vm: vm}
		channel, err := makeWaitableValue(module, "Waitable<Int>", newVMValue("Int", int64(0)))
		if err != nil {
			t.Fatal(err)
		}
		selection, err := vm.prepareChannelSelection(nil, module, []channelSelectCase{{channel: channel, send: sending, value: newVMValue("Int", int64(7))}})
		if err != nil {
			t.Fatal(err)
		}
		selection.register()
		if err := waitableCloseValue(module, channel); err != nil {
			t.Fatal(err)
		}
		if !selection.tryCommit() || (selection.err != nil) != sending {
			t.Fatalf("close did not settle selection: %+v", selection)
		}
		if !sending {
			got, err := asInt64(selection.value)
			if selection.ok || err != nil || got != 0 {
				t.Fatalf("closed receive: %v %t", selection.value, selection.ok)
			}
		}
		resource, ok := materializeManagedWaitable(channel)
		if !ok || resource.selectHead != nil {
			t.Fatal("close retained selection")
		}
	}
}
