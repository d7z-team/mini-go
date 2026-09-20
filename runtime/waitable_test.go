package runtime

import "testing"

func TestAbortTaskCancelsBlockedFFICall(t *testing.T) {
	machine := &executionMachine{}

	canceled := false
	vm := &vm{ffiCalls: map[*pendingFFICall]struct{}{}}
	call := &pendingFFICall{vm: vm, cancel: func() { canceled = true }}
	vm.ffiCalls[call] = struct{}{}
	vm.pendingEvents.Add(1)
	ffiTask := &executionTask{id: 11, blocked: &blockedOperation{kind: "ffi", ffi: call}}
	machine.abortTask(ffiTask)
	if ffiTask.blocked != nil || !canceled || call.state != pendingFFICanceled || call.vm != nil || len(vm.ffiCalls) != 0 || vm.pendingEvents.Load() != 0 {
		t.Fatalf("FFI abort retained state: task=%#v canceled=%t call=%#v calls=%d pending=%d", ffiTask.blocked, canceled, call, len(vm.ffiCalls), vm.pendingEvents.Load())
	}
}
