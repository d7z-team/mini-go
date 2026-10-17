package runtime

import (
	"errors"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// mutexResource is owned by the synchronization control transaction. A grant
// reserves a waiter without making it a lock holder until its continuation runs.
type mutexResource struct {
	vm     *vm
	ref    guestRef
	locked bool
	grant  *mutexWaiter
	head   *mutexWaiter
	tail   *mutexWaiter
}

func (module *moduleInstance) importManagedMutex(value vmValue, resource *mutexResource, batch *managedAllocationBatch) (vmValue, error) {
	ownedType, err := ownedStorageType(value.Type, module.resolvedRuntimeType)
	if err != nil {
		return vmValue{}, err
	}
	if !resource.ref.valid() {
		layout, err := module.vm.heap.registerLayout(managedLayout{stride: managedWaitableIdentityBytes})
		if err != nil {
			return vmValue{}, err
		}
		defer module.vm.heap.releaseLayout(layout)
		reservation, err := module.vm.heap.reserve(layout, 1, ir.StorageControlRecordBytes, batch)
		if err != nil {
			return vmValue{}, err
		}
		ref := reservation.commit()
		if !ref.valid() {
			return vmValue{}, errors.New("mutex publication failed")
		}
		resource.ref = ref
		module.vm.managedWaitablesMu.Lock()
		if module.vm.managedWaitables == nil {
			module.vm.managedWaitables = make(map[guestRef]*waitableResource)
		}
		module.vm.managedWaitables[ref] = &waitableResource{Type: ownedType, mutex: resource}
		module.vm.managedWaitablesMu.Unlock()
	}
	return vmValue{Type: value.Type, ref: resource.ref, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}, nil
}

type mutexWaiter struct {
	next          *mutexWaiter
	admittedBytes int64
}

type mutexLockRequest struct {
	resource *mutexResource
	waiter   *mutexWaiter
}

func (*mutexLockRequest) Error() string { return "mutex lock is pending" }

func mutexState(ctx intrinsicContext, address vmValue, create bool) (*mutexResource, error) {
	value, err := derefPointer(address)
	if err != nil {
		return nil, err
	}
	if value.payload() == nil {
		if !create {
			return nil, nil
		}
		resource := &mutexResource{vm: ctx.vm}
		value.setPayload(resource)
		if err := storePointer(address, value); err != nil {
			return nil, err
		}
		return resource, nil
	}
	resource, ok := value.payload().(*mutexResource)
	if !ok {
		return nil, errors.New("invalid mutex resource")
	}
	return resource, nil
}

func mutexLock(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	resource, err := mutexState(ctx, args[0], true)
	if err != nil {
		return nil, err
	}
	if resource.vm == nil {
		resource.vm = ctx.vm
	}
	if !resource.locked && resource.grant == nil {
		resource.locked = true
		return nil, nil
	}
	// Every waiter owns one queue slot; allocation is checked before publishing it.
	logicalBytes := int64(ir.StorageRecordHeaderBytes + ir.StorageDynamicBytes)
	if err := ctx.vm.chargeAllocationBytes(logicalBytes, ctx.batch); err != nil {
		return nil, err
	}
	waiter := &mutexWaiter{admittedBytes: logicalBytes}
	if resource.tail == nil {
		resource.head = waiter
	} else {
		resource.tail.next = waiter
	}
	resource.tail = waiter
	return nil, &mutexLockRequest{resource: resource, waiter: waiter}
}

func mutexTryLock(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	resource, err := mutexState(ctx, args[0], true)
	if err != nil {
		return nil, err
	}
	acquired := !resource.locked && resource.grant == nil
	if acquired {
		resource.locked = true
	}
	return []vmValue{newVMValue("Bool", acquired)}, nil
}

func mutexUnlock(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	resource, err := mutexState(ctx, args[0], false)
	if err != nil {
		return nil, err
	}
	if resource == nil || !resource.locked {
		return nil, newGuestPanic(errors.New("sync: unlock of unlocked mutex"))
	}
	resource.locked = false
	resource.grantNext()
	return nil, nil
}

func (resource *mutexResource) grantNext() {
	if resource.locked || resource.grant != nil || resource.head == nil {
		return
	}
	resource.grant = resource.head
	resource.head = resource.head.next
	resource.grant.next = nil
	if resource.head == nil {
		resource.tail = nil
	}
}

func (resource *mutexResource) cancel(waiter *mutexWaiter) {
	if resource.grant == waiter {
		resource.grant = nil
		waiter.release(resource.vm)
		resource.grantNext()
		return
	}
	var previous *mutexWaiter
	for queued := resource.head; queued != nil; queued = queued.next {
		if queued == waiter {
			if previous == nil {
				resource.head = queued.next
			} else {
				previous.next = queued.next
			}
			if resource.tail == queued {
				resource.tail = previous
			}
			queued.next = nil
			queued.release(resource.vm)
			return
		}
		previous = queued
	}
}

func (waiter *mutexWaiter) release(vm *vm) {
	if waiter == nil || waiter.admittedBytes == 0 {
		return
	}
	if vm != nil {
		vm.releaseAllocationBytes(waiter.admittedBytes)
	}
	waiter.admittedBytes = 0
}
