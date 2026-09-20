package runtime

import (
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestChannelSelectionReservesBeforeRegistrationAndKeepsInputRoots(t *testing.T) {
	const selectionBytes = ir.RuntimeNodeBytes + ir.RuntimeNodeBytes + 3*ir.RuntimeSlotBytes
	for _, limit := range []int64{ir.RuntimeNodeBytes + selectionBytes - 1, ir.RuntimeNodeBytes + selectionBytes} {
		vm := &vm{limits: normalizeLimits(Limits{MaxAllocatedBytes: limit})}
		vm.owner.Store(true)
		vm.allocatedSinceSweep.Store(limit)
		task := &executionTask{id: 1}
		vm.machine = &executionMachine{vm: vm, tasks: map[int64]*executionTask{1: task}}
		module := &moduleInstance{vm: vm}
		resource := &waitableResource{Type: coerceRuntimeType("Waitable<Int>")}
		channel := newVMValue(resource.Type, resource)
		selection, err := vm.prepareChannelSelection(task, module, []channelSelectCase{{channel: channel}})
		if limit < ir.RuntimeNodeBytes+selectionBytes {
			var resourceLimit ResourceLimitError
			if !errors.As(err, &resourceLimit) {
				t.Fatalf("reserve: %v", err)
			}
			if resource.selectHead != nil || resource.selectTail != nil {
				t.Fatal("failed reservation registered a case")
			}
			if vm.liveGuestBytes.Load() != ir.RuntimeNodeBytes {
				t.Fatalf("input root census: %d", vm.liveGuestBytes.Load())
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if vm.refreshLiveGuestBytes() != limit {
				t.Fatalf("selection census: %d", vm.liveGuestBytes.Load())
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
			resource := channel.Data.(*waitableResource)
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
	if channel.Data.(*waitableResource).selectHead != nil {
		t.Fatal("losing receive remained registered")
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
		if channel.Data.(*waitableResource).selectHead != nil {
			t.Fatal("close retained selection")
		}
	}
}
