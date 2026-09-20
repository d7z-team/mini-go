package runtime

import (
	"errors"
	"testing"
)

func TestMutexGrantCancellationAndDelivery(t *testing.T) {
	ctx := intrinsicContext{vm: &vm{}}
	state := newVMValue("Waitable<Bool>", nil)
	args := []vmValue{newSlotPointerValue(state.Type, "mutex", &slot{typ: state.Type, value: state, initialized: true})}
	if _, err := mutexLock(ctx, args); err != nil {
		t.Fatal(err)
	}
	waits := make([]*mutexLockRequest, 3)
	for i := range waits {
		_, err := mutexLock(ctx, args)
		if !errors.As(err, &waits[i]) {
			t.Fatalf("lock did not park: %v", err)
		}
	}
	resource := waits[0].resource
	// Remove a queued waiter before granting; its successor keeps FIFO position.
	resource.cancel(waits[1].waiter)
	if _, err := mutexUnlock(ctx, args); err != nil {
		t.Fatal(err)
	}
	if resource.grant != waits[0].waiter {
		t.Fatal("first waiter did not receive the grant")
	}
	got, err := mutexTryLock(ctx, args)
	if err != nil || got[0].Data != false {
		t.Fatalf("TryLock stole a pending grant: %v %v", got, err)
	}
	if _, err := mutexUnlock(ctx, args); err == nil {
		t.Fatal("undelivered grant became an owned lock")
	}
	resource.cancel(waits[0].waiter)
	if resource.grant != waits[2].waiter {
		t.Fatal("canceled grant was not handed to next waiter")
	}
	frame := &executionFrame{frame: &frame{}}
	task := &executionTask{frames: []*executionFrame{frame}}
	machine := &executionMachine{vm: ctx.vm}
	ready, err := machine.resumeBlocked(task, &blockedOperation{kind: "mutex", mutex: waits[2]})
	if err != nil || !ready || !resource.locked || resource.grant != nil {
		t.Fatalf("grant delivery: %v %v", ready, err)
	}
	resource.cancel(waits[2].waiter)
	if !resource.locked {
		t.Fatal("cancellation released an already delivered lock")
	}
	if _, err := mutexUnlock(ctx, args); err != nil {
		t.Fatal(err)
	}
	got, err = mutexTryLock(ctx, args)
	if err != nil || got[0].Data != true {
		t.Fatalf("mutex could not be reused: %v %v", got, err)
	}
}

func TestMutexWaitRetainsResourceWithoutSourceReference(t *testing.T) {
	waiter := &mutexWaiter{}
	resource := &mutexResource{grant: waiter}
	task := &executionTask{blocked: &blockedOperation{kind: "mutex", mutex: &mutexLockRequest{resource: resource, waiter: waiter}}}
	sizer := newRuntimeValueSizer()
	sizer.task(task)
	if sizer.bytes < 272 {
		t.Fatalf("pending mutex resource was not accounted: %d", sizer.bytes)
	}
	(&executionMachine{}).cancelBlockedOperation(task)
	if resource.grant != nil || task.blocked != nil {
		t.Fatal("cancel retained mutex registration")
	}
}
