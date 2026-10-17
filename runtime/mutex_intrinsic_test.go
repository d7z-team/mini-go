package runtime

import (
	"errors"
	"testing"
)

func TestManagedMutexIdentitySurvivesCollection(t *testing.T) {
	module := newManagedValueTestModule()
	module.vm.revision.Store(&instanceRevision{root: module})
	ctx := intrinsicContext{vm: module.vm, module: module}
	typ := coerceRuntimeType("Waitable<Bool>")
	structureType := coerceRuntimeType("struct{state:Waitable<Bool>}")
	cell := newSlot(structureType, module, false)
	cell.publish(module.adoptValue(newRuntimeStructValue(module, structureType, map[string]vmValue{
		"state": newVMValue(typ, nil),
	})))
	address := newTargetPointer(&vmPointer{
		Type: typ, target: pointerField, module: module,
		parent: newSlotPointerValue(structureType, "mutex", cell), field: "state",
	})
	args := []vmValue{address}
	if _, err := mutexLock(ctx, args); err != nil {
		t.Fatal(err)
	}
	value, err := derefPointer(address)
	if err != nil {
		t.Fatal(err)
	}
	if resource, ok := value.payload().(*mutexResource); !ok || !resource.locked {
		t.Fatalf("mutex state was not published: %s (%T)", value.Type, value.payload())
	}
	if _, err := module.vm.heap.collect([]guestRef{address.ref}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := mutexUnlock(ctx, args); err != nil {
		t.Fatal(err)
	}
	values, err := mutexTryLock(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if locked, ok := values[0].boolValue(); !ok || !locked {
		t.Fatal("mutex could not be acquired again")
	}
}

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
