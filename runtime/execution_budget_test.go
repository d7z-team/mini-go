package runtime

import (
	"encoding/json"
	"math"
	"os"
	goruntime "runtime"
	"strconv"
	"sync"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestTasksShareAnExactConcurrentStepLimit(t *testing.T) {
	const limit = 10003
	for _, workers := range []int{1, 2, 4} {
		budget := &executionBudget{}
		counts := make([]int, workers)
		errors := make([]error, workers)
		var group sync.WaitGroup
		for worker := range workers {
			group.Go(func() {
				task := &executionTask{budget: budget}
				defer task.releaseStepGrant()
				for {
					if err := task.consumeStep(limit); err != nil {
						if err == errStepBudgetReserved {
							goruntime.Gosched()
							continue
						}
						errors[worker] = err
						return
					}
					counts[worker]++
				}
			})
		}
		group.Wait()
		total := 0
		for worker, count := range counts {
			total += count
			if _, ok := errors[worker].(StepLimitError); !ok {
				t.Fatalf("worker %d limit error: %v", worker, errors[worker])
			}
		}
		if total != limit || budget.steps.Load() != limit || budget.profilePhase.Load() != limit {
			t.Fatalf("%d workers: executed=%d charged=%d profile=%d", workers, total, budget.steps.Load(), budget.profilePhase.Load())
		}
	}
}

func TestStepGrantReturnsUnusedAllowanceWithoutChargingIt(t *testing.T) {
	budget := &executionBudget{}
	first := &executionTask{budget: budget}
	second := &executionTask{budget: budget}
	t.Cleanup(first.releaseStepGrant)
	t.Cleanup(second.releaseStepGrant)
	if err := first.consumeStep(4); err != nil {
		t.Fatal(err)
	}
	if err := second.consumeStep(4); err != errStepBudgetReserved {
		t.Fatalf("another task's unspent grant must yield, got %v", err)
	}
	if budget.steps.Load() != 1 {
		t.Fatal("reservation was reported as execution")
	}
	first.releaseStepGrant()
	for range 3 {
		if err := second.consumeStep(4); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := second.consumeStep(4).(StepLimitError); !ok {
		t.Fatal("exhausted committed allowance did not report the step limit")
	}
	if budget.steps.Load() != 4 || budget.reserved != 0 || budget.committed != 4 {
		t.Fatalf("grant settlement: steps=%d reserved=%d committed=%d", budget.steps.Load(), budget.reserved, budget.committed)
	}
}

func TestReservedStepAllowanceParksDriverUntilGrantReturns(t *testing.T) {
	artifact := ir.NewArtifact("budget/handoff", "main")
	artifact.Functions = []ir.Function{{ID: "fn.main", Signature: testSignature("function()"), Instructions: []ir.Instruction{
		{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
		{Op: string(ir.OpPop)},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	}}}
	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vm.closeRevisions)
	vm.maxSteps = 4
	if _, err := vm.prepareFunction("fn.main", nil); err != nil {
		t.Fatal(err)
	}
	peer := &executionTask{budget: vm.machine.foreground.budget}
	t.Cleanup(peer.releaseStepGrant)
	if err := peer.consumeStep(4); err != nil {
		t.Fatal(err)
	}
	outcome := vm.runPrepared(64)
	if outcome.state != ExecutionPending || outcome.executed != 0 {
		t.Fatalf("reserved allowance did not park: %+v", outcome)
	}
	peer.releaseStepGrant()
	select {
	case <-vm.wake:
	default:
		t.Fatal("returning allowance did not wake the parked driver")
	}
	outcome = vm.runPrepared(64)
	if outcome.state != ExecutionCompleted || outcome.executed != 3 {
		t.Fatalf("resumed execution: %+v", outcome)
	}
}

func TestSharedStepLimits(t *testing.T) {
	data, err := os.ReadFile("../testdata/runtime/step_limits.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input, Normalized string
		Error             bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Input, func(t *testing.T) {
			input, err := strconv.ParseInt(test.Input, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			limits := Limits{MaxSteps: input}
			if err := validateLimits(limits); (err != nil) != test.Error {
				t.Fatal(err)
			}
			if !test.Error && strconv.FormatInt(normalizeLimits(limits).MaxSteps, 10) != test.Normalized {
				t.Fatal("normalization mismatch")
			}
		})
	}
}

func TestUnlimitedStepsPreservePollCountsAndSampling(t *testing.T) {
	artifact := ir.NewArtifact("budget/loop", "main")
	artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function() Void"), Instructions: []ir.Instruction{
		{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "loop"})},
		{Op: string(ir.OpJump), Payload: testPayload(ir.JumpPayload{Label: "loop"})},
	}}}
	instance, err := patchTestProgram(t, artifact, "budget-loop").Instantiate(t.Context(), InstanceOptions{Limits: Limits{MaxSteps: UnlimitedSteps}, GuestProfile: GuestProfileOptions{SampleEvery: 4, MaxEntries: 8}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.vm.enterOwner(); err != nil {
		t.Fatal(err)
	}
	instance.vm.executedSteps = math.MaxInt64 - 1
	instance.vm.machine.foreground.budget.steps.Store(math.MaxInt64 - 1)
	instance.vm.leaveOwner()
	for range 3 {
		state, steps, err := execution.PollSteps(7)
		if err != nil || state != ExecutionRunning || steps != 7 {
			t.Fatalf("poll = %s %d %v", state, steps, err)
		}
	}
	stats, err := execution.ScopeStats(t.Context())
	if err != nil || stats.Steps != math.MaxInt64 {
		t.Fatalf("stats = %#v, %v", stats, err)
	}
	var sampled uint64
	for _, sample := range execution.GuestProfile().Samples {
		sampled += sample.Count
	}
	if sampled != 5 {
		t.Fatalf("saturated total changed sample interval: %d", sampled)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
}
