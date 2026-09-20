package runtime

import (
	"errors"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestReflectSelectWaitingReportsCapturedRevision(t *testing.T) {
	program := patchTestProgram(t, patchGlobalArtifact(1), "select-revision")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	module := instance.vm.rootModule()
	task := &executionTask{id: 9, blocked: &blockedOperation{kind: "select", selection: &channelSelection{
		cases: []channelSelectCase{{send: true, value: newVMValue("Function", functionRef{exact: module, FunctionID: "fn.entry"})}},
	}}}
	instance.vm.machine = &executionMachine{vm: instance.vm, blocked: []*executionTask{task}}
	instance.vm.machine.addTask(task)
	t.Cleanup(func() { instance.vm.machine = nil })
	roots, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, root := range roots.Roots {
		found = found || root.TaskID == 9 && strings.Contains(root.Path, "/select/case 0/value")
	}
	if !roots.Complete || !found {
		t.Fatalf("missing pending send root: %+v", roots)
	}
	instance.vm.machine.cancelBlockedOperation(task)
	roots, err = instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots.Roots {
		if strings.Contains(root.Path, "/select/") {
			t.Fatalf("canceled wait retained root: %+v", root)
		}
	}
}

func TestReflectSelectWaitingOwnsSendValueUntilCancellation(t *testing.T) {
	instanceProgram := patchTestProgram(t, patchGlobalArtifact(1), "reflect-select-roots")
	instance, err := instanceProgram.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	module := instance.vm.rootModule()
	channel, err := makeWaitableValue(module, "Waitable<Slice<Uint8>>", newVMValue("Int", int64(0)))
	if err != nil {
		t.Fatal(err)
	}
	wrap := func(value vmValue) vmValue {
		out := zeroReflectValueValue()
		setRuntimeStructField(out.Data.(*vmStruct), "valid", newBoolValue(true))
		setRuntimeStructField(out.Data.(*vmStruct), "data", newVMValue("Any", value))
		return out
	}
	const size = 65536
	send := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, size), size, size)
	selected := newRuntimeStructValue(nil, "reflect.SelectCase", map[string]vmValue{
		"Dir": newVMValue("Int", int64(reflectSelectSend)), "Chan": wrap(channel), "Send": wrap(send),
	})
	_, err = reflectSelect(intrinsicContext{vm: instance.vm, module: module}, []vmValue{newSliceValue("Slice<reflect.SelectCase>", []vmValue{selected})})
	var request *reflectSelectRequest
	if !errors.As(err, &request) {
		t.Fatalf("expected blocked selection, got %v", err)
	}
	resource := channel.Data.(*waitableResource)
	if request.selection.done || resource.bufferLen() != 0 {
		t.Fatal("select sent before choosing a case")
	}
	task := &executionTask{blocked: &blockedOperation{kind: "select", selection: request.selection, reflectSelect: request}}
	sizer := newRuntimeValueSizer()
	sizer.task(task)
	if !sizer.seenStorage[send.Data.(*vmSlice).vmSliceStorage] || sizer.bytes < size*ir.RuntimeByteBytes {
		t.Fatal("waiting send storage missing from census")
	}
	instance.vm.machine.cancelBlockedOperation(task)
	if task.blocked != nil || resource.selectHead != nil {
		t.Fatal("cancel retained waiting registration")
	}
	sizer = newRuntimeValueSizer()
	sizer.task(task)
	if sizer.bytes != 0 {
		t.Fatalf("canceled task retained %d bytes", sizer.bytes)
	}
}
