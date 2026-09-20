package runtime

import (
	"errors"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// A selection owns all of its registrations. A transaction either commits a
// single winner (including both peers of a rendezvous), or publishes all cases.
type channelSelection struct {
	vm        *vm
	cases     []channelSelectCase
	index     int
	done      bool
	value     vmValue
	ok        bool
	err       error
	owner     *taskOwnership
	ticket    *taskWaitTicket
	allocated bool
}

type channelSelectCase struct {
	channel        vmValue
	resource       *waitableResource
	send           bool
	value          vmValue
	zero           vmValue
	selection      *channelSelection
	index          int
	previous, next *channelSelectCase
	registered     bool
}

func (vm *vm) prepareChannelSelection(task *executionTask, module *moduleInstance, cases []channelSelectCase) (*channelSelection, error) {
	selection := &channelSelection{vm: vm, cases: cases, index: -1}
	if task != nil {
		task.preparingSelection = selection
	}
	if _, _, err := vm.checkCollectionSize(int64(len(cases)), int64(len(cases))); err != nil {
		return nil, err
	}
	if err := vm.chargeAllocationBytes(ir.RuntimeNodeBytes + int64(len(cases))*(ir.RuntimeNodeBytes+3*ir.RuntimeSlotBytes)); err != nil {
		return nil, err
	}
	selection.allocated = true
	for index := range selection.cases {
		selected := &selection.cases[index]
		selected.selection, selected.index = selection, index
		resource, err := waitableValueData(module, selected.channel)
		if err != nil {
			return nil, err
		}
		selected.resource = resource
		if selected.send {
			if !module.waitableCanSend(selected.channel.Type) {
				return nil, fmt.Errorf("cannot send on receive-only waitable %s", selected.channel.Type)
			}
			if resource != nil {
				selected.value, err = normalizeWaitableSendValue(module, resource, selected.value)
				if err != nil {
					return nil, err
				}
			}
		} else {
			if !module.waitableCanRecv(selected.channel.Type) {
				return nil, fmt.Errorf("cannot receive from send-only waitable %s", selected.channel.Type)
			}
			selected.zero = waitableZeroValue(module, selected.channel.Type, resource)
		}
	}
	return selection, nil
}

func (selection *channelSelection) deliver(frame *frame, payload *ir.SelectPayload, withOK bool) error {
	if selection.err != nil {
		return selection.err
	}
	if payload != nil {
		if err := frame.localCells[frame.function.LocalIndexes[payload.Index]].store(newVMValue("Int", int64(selection.index))); err != nil {
			return err
		}
		if selection.index >= 0 {
			selected := payload.Cases[selection.index]
			if selected.Send == "" {
				if err := frame.localCells[frame.function.LocalIndexes[selected.Value]].store(selection.value); err != nil {
					return err
				}
				return frame.localCells[frame.function.LocalIndexes[selected.OK]].store(newVMValue("Bool", selection.ok))
			}
		}
	} else if len(selection.cases) == 1 && !selection.cases[0].send {
		frame.push(selection.value)
		if withOK {
			frame.push(newVMValue("Bool", selection.ok))
		}
	}
	return nil
}

func (selected *channelSelectCase) peer() *channelSelectCase {
	return selected.resource.selectionPeer(selected.send, selected.selection)
}

func (resource *waitableResource) selectionPeer(send bool, owner *channelSelection) *channelSelectCase {
	for peer := resource.selectHead; peer != nil; peer = peer.next {
		if peer.selection != owner && peer.send != send && !peer.selection.done {
			return peer
		}
	}
	return nil
}

func (selection *channelSelection) tryCommit() bool {
	if selection.done {
		return true
	}
	var ready []int
	for index := range selection.cases {
		selected := &selection.cases[index]
		resource := selected.resource
		if resource == nil {
			continue
		}
		available := resource.Closed
		if selected.send {
			available = available || selected.peer() != nil || resource.Capacity > resource.bufferLen()
		} else {
			available = available || resource.bufferLen() != 0 || selected.peer() != nil
		}
		if available {
			ready = append(ready, index)
		}
	}
	index := selection.vm.chooseReadyIndex(ready)
	if index < 0 {
		return false
	}
	selected := &selection.cases[index]
	resource := selected.resource
	if selected.send {
		if resource.Closed {
			selection.err = newGuestPanic(errors.New("send on closed waitable"))
			selection.complete(index, vmValue{}, false)
		} else if peer := selected.peer(); peer != nil {
			peer.selection.complete(peer.index, selected.value, true)
			selection.complete(index, vmValue{}, false)
		} else {
			resource.appendBuffer(selected.value)
			selection.complete(index, vmValue{}, false)
		}
	} else if resource.bufferLen() != 0 {
		value := resource.popBuffer()
		selection.complete(index, value, true)
	} else if peer := selected.peer(); peer != nil && !resource.Closed {
		value := peer.value
		peer.selection.complete(peer.index, vmValue{}, false)
		selection.complete(index, value, true)
	} else {
		selection.complete(index, selected.zero, false)
	}
	return true
}

func (selection *channelSelection) complete(index int, value vmValue, ok bool) {
	selection.unregister()
	selection.index, selection.value, selection.ok, selection.done = index, value, ok, true
	if selection.owner != nil {
		selection.owner.notify(selection.ticket)
	}
}

func (selection *channelSelection) register() {
	for index := range selection.cases {
		selected := &selection.cases[index]
		resource := selected.resource
		if resource == nil {
			continue
		}
		selected.previous = resource.selectTail
		if resource.selectTail == nil {
			resource.selectHead = selected
		} else {
			resource.selectTail.next = selected
		}
		resource.selectTail = selected
		selected.registered = true
	}
}

func (selection *channelSelection) unregister() {
	for index := range selection.cases {
		selected := &selection.cases[index]
		if !selected.registered {
			continue
		}
		resource := selected.resource
		if selected.previous == nil {
			resource.selectHead = selected.next
		} else {
			selected.previous.next = selected.next
		}
		if selected.next == nil {
			resource.selectTail = selected.previous
		} else {
			selected.next.previous = selected.previous
		}
		selected.previous, selected.next, selected.registered = nil, nil, false
	}
}
