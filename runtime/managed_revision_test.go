package runtime

import "testing"

func TestPatchAllocationFailurePreservesCurrentRevisionAndGlobals(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "allocation-base")
	next := patchTestProgram(t, patchGlobalArtifact(2), "allocation-next")
	instance, err := base.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Call(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	plan, err := instance.PreparePatch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	machine := instance.vm
	before := instance.Revision()
	cell := machine.rootModule().globalCells[0]
	value := cell.load()
	anchor := machine.revision.Load().anchor
	stats := machine.heap.stats()
	machine.heap.maxGuestBytes = stats.occupiedBytes
	if _, err := instance.ApplyPatch(plan); patchErrorCode(err) != "resource_limit" {
		t.Fatalf("patch allocation error = %v", err)
	}
	machine.heap.maxGuestBytes = 0
	if instance.Revision() != before || machine.revision.Load().anchor != anchor || machine.rootModule().globalCells[0] != cell {
		t.Fatal("failed patch changed revision or global identity")
	}
	if got := cell.load(); !got.Type.Equal(value.Type) || got.scalar != value.scalar || got.ref != value.ref {
		t.Fatal("failed patch changed global value")
	}
	after := machine.heap.stats()
	if after.occupiedBytes != stats.occupiedBytes || after.reservedBytes != stats.reservedBytes {
		t.Fatalf("failed patch retained storage: before=%+v after=%+v", stats, after)
	}
	if _, err := instance.Call(t.Context(), "run"); err != nil {
		t.Fatalf("current code failed after patch allocation rejection: %v", err)
	}
}

func TestInstanceCloseReleasesManagedPagesAndMetadata(t *testing.T) {
	program := patchTestProgram(t, patchClosureArtifact(3), "managed-close")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Call(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	machine := instance.vm
	before := machine.heap.stats()
	if before.residentPageBytes == 0 {
		t.Fatal("execution did not allocate managed storage")
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	after := machine.heap.stats()
	if after.occupiedBytes != 0 || after.reservedBytes != 0 || after.residentPageBytes != 0 {
		t.Fatalf("closed instance retained managed storage: %+v", after)
	}
	if len(machine.managedFunctions)+len(machine.managedWaitables)+len(machine.managedRevisions) != 0 || machine.valueLayouts != nil {
		t.Fatal("closed instance retained managed metadata")
	}
}
