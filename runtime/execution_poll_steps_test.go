package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/d7z-team/mini-go/ffi"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestExecutionPollStepsUsesExactSliceBudget(t *testing.T) {
	artifact := ir.NewArtifact("scheduler/poll-steps", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			{Op: string(ir.OpPop)},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
		},
	}}
	instance, err := patchTestProgram(t, artifact, "poll-steps").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, executed, pollErr := execution.PollSteps(1)
	instance.vm.leaveOwner()
	if !errors.Is(pollErr, ErrBusy) || executed != 0 || state != ExecutionRunning || execution.State() != ExecutionRunning {
		t.Fatalf("busy PollSteps = %s, %d, %v", state, executed, pollErr)
	}
	for _, invalid := range []int{0, -1} {
		state, executed, pollErr := execution.PollSteps(invalid)
		if pollErr == nil || state != ExecutionRunning || executed != 0 {
			t.Fatalf("PollSteps(%d) = %s, %d, %v", invalid, state, executed, pollErr)
		}
	}
	for call := 1; call <= 3; call++ {
		state, executed, pollErr := execution.PollSteps(1)
		want := ExecutionRunning
		if call == 3 {
			want = ExecutionCompleted
		}
		if pollErr != nil || state != want || executed != 1 {
			t.Fatalf("slice %d = %s, %d, %v; want %s, 1, nil", call, state, executed, pollErr, want)
		}
	}
	scope, err := execution.ScopeStats(context.Background())
	if err != nil || scope.Steps != 3 {
		t.Fatalf("scope after slices = %#v, %v", scope, err)
	}
}

func TestExecutionPollStepsPreservesCumulativeScopeLimit(t *testing.T) {
	artifact := ir.NewArtifact("scheduler/poll-limit", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			{Op: string(ir.OpPop)},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
		},
	}}
	instance, err := patchTestProgram(t, artifact, "poll-limit").Instantiate(context.Background(), InstanceOptions{Limits: Limits{MaxSteps: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if state, executed, err := execution.PollSteps(1); err != nil || state != ExecutionRunning || executed != 1 {
			t.Fatalf("limited slice = %s, %d, %v", state, executed, err)
		}
	}
	state, executed, err := execution.PollSteps(1)
	var limit StepLimitError
	if state != ExecutionFailed || executed != 0 || !errors.As(err, &limit) || limit.MaxSteps != 2 {
		t.Fatalf("limit slice = %s, %d, %T %v", state, executed, err, err)
	}
}

func TestExecutionPollStepsPendingFFIReturnsZeroSteps(t *testing.T) {
	bridge := ffi.CallFunc(func(context.Context, ffi.Request, ffi.Completion) (ffi.Call, error) {
		return ffi.CancelFunc(func() {}), nil
	})
	instance, err := patchTestProgram(t, patchFFIArtifact(), "poll-ffi").Instantiate(context.Background(), InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	state, executed, err := execution.PollSteps(3)
	if err != nil || state != ExecutionPending || executed != 3 {
		t.Fatalf("FFI blocking slice = %s, %d, %v", state, executed, err)
	}
	state, executed, err = execution.PollSteps(1)
	if err != nil || state != ExecutionPending || executed != 0 {
		t.Fatalf("FFI pending slice = %s, %d, %v", state, executed, err)
	}
}
