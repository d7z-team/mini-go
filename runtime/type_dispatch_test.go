package runtime

import "testing"

func TestTypeDispatchRetainsSubjectAcrossCandidateSteps(t *testing.T) {
	module := &moduleInstance{}
	subject := &slot{typ: newVMValue("Any", nil).Type, module: module}
	subject.publish(newVMValue("Any", newVMValue("Int", int64(42))))
	result := &slot{typ: newVMValue("Int", nil).Type, module: module}
	call := &frame{module: module, pc: 1, localCells: []*slot{subject, result}}
	inst := &preparedInstruction{op: preparedTypeDispatch, dispatch: &preparedTypeSwitch{
		subject: 0, fallback: 10, fallbackLocal: -1,
		cases: []preparedTypeCase{
			{typ: newVMValue("String", nil).Type, target: 10, binding: -1},
			{typ: result.typ, target: 20, binding: 1},
		},
	}}
	vm := &vm{}
	if err := vm.executeInstructionBody(nil, call, inst); err != nil {
		t.Fatal(err)
	}
	if call.pc != 0 || call.typeDispatchIndex != 1 || !call.typeDispatchActive {
		t.Fatalf("candidate did not suspend: pc=%d index=%d active=%v", call.pc, call.typeDispatchIndex, call.typeDispatchActive)
	}
	// A shared subject cell may change while a different task runs.
	subject.publish(newVMValue("Any", newVMValue("String", "changed")))
	call.pc++
	if err := vm.executeInstructionBody(nil, call, inst); err != nil {
		t.Fatal(err)
	}
	if got, err := numericAsInt64(result.load()); err != nil || got != 42 || call.pc != 20 {
		t.Fatalf("dispatch observed a second subject: value=%v pc=%d err=%v", got, call.pc, err)
	}
	if call.typeDispatchActive || call.typeDispatchValue.Data != nil || call.typeDispatchIndex != 0 {
		t.Fatal("completed dispatch retained its continuation")
	}
}
