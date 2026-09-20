package runtime

import "errors"

// mutexResource is owned by the synchronization control transaction. A grant
// reserves a waiter without making it a lock holder until its continuation runs.
type mutexResource struct {
	locked bool
	grant  *mutexWaiter
	head   *mutexWaiter
	tail   *mutexWaiter
}

type mutexWaiter struct{ next *mutexWaiter }

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
	if value.Data == nil {
		if !create {
			return nil, nil
		}
		if err := ctx.vm.chargeRuntimeObject(0, 0); err != nil {
			return nil, err
		}
		resource := &mutexResource{}
		value.Data = resource
		if err := storePointer(address, value); err != nil {
			return nil, err
		}
		return resource, nil
	}
	resource, ok := value.Data.(*mutexResource)
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
	if !resource.locked && resource.grant == nil {
		resource.locked = true
		return nil, nil
	}
	// Every waiter owns one queue slot; allocation is checked before publishing it.
	if err := ctx.vm.chargeRuntimeObject(1, 0); err != nil {
		return nil, err
	}
	waiter := &mutexWaiter{}
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
			return
		}
		previous = queued
	}
}
