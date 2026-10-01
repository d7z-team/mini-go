package runtime

import (
	"context"
	"testing"
)

func TestGuestProfileIsBoundedAndRevisionTagged(t *testing.T) {
	artifact := patchCallArtifact(1, 2)
	for functionIndex := range artifact.Functions {
		for instructionIndex := range artifact.Functions[functionIndex].Code.Instructions {
			setTestInstructionLocations(t, &artifact, testInstructionLocation{
				function: artifact.Functions[functionIndex].ID, pc: instructionIndex, line: instructionIndex + 1, column: 1,
			})
		}
	}
	program := patchTestProgram(t, artifact, "profile")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{
		GuestProfile: GuestProfileOptions{SampleEvery: 1, MaxEntries: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	profile := execution.GuestProfile()
	if profile.SampleEvery != 1 || len(profile.Samples) != 2 || profile.Dropped == 0 {
		t.Fatalf("profile = %#v", profile)
	}
	for _, sample := range profile.Samples {
		if sample.Generation != 1 || sample.Module != artifact.Module.Path || sample.FunctionID == "" || sample.Opcode == "" || sample.Location.File != "main.mgo" {
			t.Fatalf("incomplete sample: %#v", sample)
		}
	}
}

func TestGuestProfileOptionsRequirePowerOfTwoInterval(t *testing.T) {
	program := patchTestProgram(t, patchCallArtifact(1, 2), "profile-options")
	if _, err := program.Instantiate(context.Background(), InstanceOptions{
		GuestProfile: GuestProfileOptions{SampleEvery: 3},
	}); err == nil {
		t.Fatal("non-power-of-two sample interval was accepted")
	}
}
