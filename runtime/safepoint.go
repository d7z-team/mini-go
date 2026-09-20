package runtime

import (
	"context"
	"errors"
)

func (vm *vm) enterOwner() error {
	if vm == nil {
		return errors.New("nil VM")
	}
	vm.leaseMu.Lock()
	defer vm.leaseMu.Unlock()
	if vm.controlWaiters.Load() != 0 || vm.activeSlices != 0 || !vm.owner.CompareAndSwap(false, true) {
		return ErrBusy
	}
	return nil
}

func (vm *vm) enterOwnerContext(ctx context.Context) error {
	if vm == nil {
		return errors.New("nil VM")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Only the external caller waits. Executor control jobs retain a request
	// and return their worker until the last running lease acknowledges it.
	request := vm.requestControl()
	defer request.cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if request.acquire() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-vm.ownerWake:
		}
	}
}

func (vm *vm) leaveOwner() {
	if vm != nil {
		vm.leaseMu.Lock()
		vm.owner.Store(false)
		vm.leaseMu.Unlock()
		vm.notifyControlReady()
	}
}

// publishSlices atomically converts the full owner lease into count execution
// leases. The selected tasks already own their continuations, so no control
// request can observe an unrooted gap between selection and worker execution.
func (vm *vm) publishSlices(count int) {
	if vm == nil || count <= 0 {
		panic("runtime: invalid execution slice publication")
	}
	vm.leaseMu.Lock()
	defer vm.leaseMu.Unlock()
	if !vm.owner.Load() || vm.activeSlices != 0 {
		panic("runtime: execution slices require the full owner")
	}
	vm.activeSlices = count
	vm.owner.Store(false)
}

// controlRequest stops new dispatch while existing leases return their roots.
// It belongs to one host caller or control job; it must be acquired or canceled.
type controlRequest struct {
	vm      *vm
	pending bool
}

func (vm *vm) requestControl() *controlRequest {
	vm.leaseMu.Lock()
	vm.controlWaiters.Add(1)
	vm.leaseMu.Unlock()
	return &controlRequest{vm: vm, pending: true}
}

func (request *controlRequest) acquire() bool {
	if !request.pending {
		return false
	}
	vm := request.vm
	vm.leaseMu.Lock()
	acquired := vm.activeSlices == 0 && vm.owner.CompareAndSwap(false, true)
	vm.leaseMu.Unlock()
	if acquired {
		request.cancel()
	}
	return acquired
}

func (request *controlRequest) cancel() {
	if !request.pending {
		return
	}
	request.pending = false
	vm := request.vm
	vm.controlWaiters.Add(-1)
	// Pass the notification to another requester even when this caller was
	// canceled after consuming the last worker's acknowledgement.
	vm.notifyControlReady()
}

// acquireSlice publishes a task lease before its private roots may be mutated.
// Control acquisition and dispatch use the same gate: neither can pass a stale
// observation of the other's ownership. Waiting is left to the external host;
// an executor job that cannot enter simply returns its worker.
func (vm *vm) acquireSlice() bool {
	vm.leaseMu.Lock()
	defer vm.leaseMu.Unlock()
	if vm.owner.Load() || vm.controlWaiters.Load() != 0 {
		return false
	}
	vm.activeSlices++
	return true
}

// releaseSlice is called after the task continuation and roots have been
// published. The last acknowledgement wakes control without waiting for it.
func (vm *vm) releaseSlice() {
	vm.leaseMu.Lock()
	if vm.activeSlices == 0 {
		vm.leaseMu.Unlock()
		panic("runtime: release without an execution lease")
	}
	vm.activeSlices--
	ready := vm.activeSlices == 0
	vm.leaseMu.Unlock()
	if ready {
		vm.notifyControlReady()
	}
}

func (vm *vm) notifyControlReady() {
	select {
	case vm.ownerWake <- struct{}{}:
	default:
	}
	if instance := vm.instance; instance != nil && !vm.owner.Load() && instance.supervisorRetry.Swap(false) {
		instance.signalSupervisor()
	}
}
