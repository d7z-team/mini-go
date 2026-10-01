package bytecode

import "testing"

func TestWaitableOpcodeContract(t *testing.T) {
	for _, opcode := range []Opcode{
		OpMakeWaitable,
		OpWaitableSend,
		OpWaitableRecv,
		OpWaitableRecvOK,
		OpWaitableCanRecv,
		OpWaitableTryRecv,
		OpWaitableTrySend,
		OpWaitableCanSend,
		OpWaitableClose,
	} {
		if !IsKnownOpcode(opcode) {
			t.Fatalf("waitable opcode %q is not registered", opcode)
		}
		spec, ok := OpcodeSpecFor(opcode)
		if !ok || spec.Category != "waitable" && opcode != OpMakeWaitable {
			t.Fatalf("unexpected waitable opcode spec for %q: %#v", opcode, spec)
		}
	}
}
