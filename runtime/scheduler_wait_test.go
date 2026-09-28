package runtime

import (
	"math/rand"
	"sort"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestBlockedContextProjectionIsBoundedAndStable(t *testing.T) {
	machine := &executionMachine{}
	for _, i := range rand.New(rand.NewSource(1)).Perm(1000) {
		machine.blocked = append(machine.blocked, &executionTask{
			scope: &executionScope{id: int64(i % 2)},
			blocked: &blockedOperation{error: Error{
				ExecutionContextID: int64(i / 2), Generation: 2, ProgramHash: "revision", ModulePath: "example/blocked",
				FunctionID: "fn.wait", PC: 3, Op: string(ir.OpWaitableRecv), Err: WaitBlockedError{Message: "waiting"},
			}},
		})
	}
	machine.blocked = append(machine.blocked, nil, &executionTask{})
	total, contexts := machine.blockedContextSnapshot()
	if total != 1000 || len(contexts) != maxBlockedContexts || cap(contexts) != maxBlockedContexts {
		t.Fatalf("projection total=%d len=%d cap=%d", total, len(contexts), cap(contexts))
	}
	for i, context := range contexts {
		if context.ExecutionContextID != int64(i/2) || context.ScopeID != int64(i%2) || context.Reason != "waiting" {
			t.Fatalf("context %d = %+v", i, context)
		}
		if context.Revision.Generation != 2 || context.Revision.Hash != "revision" || context.ModulePath != "example/blocked" || context.FunctionID != "fn.wait" || context.PC != 3 || context.Op != string(ir.OpWaitableRecv) {
			t.Fatalf("context %d lost source identity: %+v", i, context)
		}
	}
}

func FuzzBlockedContextProjection(f *testing.F) {
	f.Add([]byte{9, 0, 2, 1, 0, 3})
	f.Fuzz(func(t *testing.T, ids []byte) {
		if len(ids) > 256 {
			ids = ids[:256]
		}
		machine := &executionMachine{}
		for _, id := range ids {
			machine.blocked = append(machine.blocked, &executionTask{blocked: &blockedOperation{error: Error{ExecutionContextID: int64(id)}}})
		}
		total, contexts := machine.blockedContextSnapshot()
		want := append([]byte(nil), ids...)
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if total != len(ids) || len(contexts) != min(len(ids), maxBlockedContexts) || cap(contexts) > maxBlockedContexts {
			t.Fatal("invalid projection size")
		}
		for i, context := range contexts {
			if context.ExecutionContextID != int64(want[i]) {
				t.Fatal("unstable projection order")
			}
		}
	})
}
