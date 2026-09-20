package runtime

import (
	"context"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func FuzzExecutionScopeLifecycle(f *testing.F) {
	f.Add([]byte{0, 2, 4, 6, 1, 3, 5})
	f.Add([]byte{0, 0, 1, 1, 6})
	f.Fuzz(func(t *testing.T, operations []byte) {
		vm := &vm{}
		machine := &executionMachine{vm: vm}
		vm.machine = machine
		scope := machine.newScope(1, 1, false, nil)
		var tasks []*executionTask
		for _, operation := range operations {
			switch operation % 7 {
			case 0:
				task := &executionTask{id: int64(len(tasks) + 1), scope: scope, budget: scope.budget}
				machine.addTask(task)
				tasks = append(tasks, task)
			case 1:
				if len(tasks) != 0 {
					machine.finishTask(tasks[int(operation)%len(tasks)], nil)
				}
			case 2:
				machine.addScopeTimer(scope)
			case 3:
				machine.releaseScopeTimer(scope)
			case 4:
				machine.addScopeFFI(scope)
			case 5:
				machine.releaseScopeFFI(scope)
			case 6:
				machine.publishScopeRoot(scope.id, ExecutionCompleted, nil)
			}
			if scope.tasks < 0 || scope.timers < 0 || scope.ffiCalls < 0 {
				t.Fatalf("negative scope counts: %#v", scope.snapshot())
			}
		}
		machine.publishScopeRoot(scope.id, ExecutionCompleted, nil)
		for _, task := range tasks {
			machine.finishTask(task, nil)
		}
		for scope.timers > 0 {
			machine.releaseScopeTimer(scope)
		}
		for scope.ffiCalls > 0 {
			machine.releaseScopeFFI(scope)
		}
		machine.cancelScope(scope, context.Canceled)
		if !scope.settled || machine.scope(scope.id) != nil {
			t.Fatalf("scope did not settle: %#v", scope.snapshot())
		}
		select {
		case <-scope.done:
		default:
			t.Fatal("settled scope did not close done")
		}
	})
}

func FuzzExecutionPollStepsAndPatch(f *testing.F) {
	f.Add([]byte{1, 2, 19, 4, 37, 8})
	f.Add([]byte{255})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 64 {
			operations = operations[:64]
		}
		artifact := ir.NewArtifact("scheduler/fuzz-poll", "main")
		instructions := make([]ir.Instruction, 0, 33)
		for range 16 {
			instructions = append(instructions,
				ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
				ir.Instruction{Op: string(ir.OpPop)},
			)
		}
		instructions = append(instructions, ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})})
		artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function() Void"), Instructions: instructions}}
		programs := []*Program{patchTestProgram(t, artifact, "fuzz-a"), patchTestProgram(t, artifact, "fuzz-b")}
		instance, err := programs[0].Instantiate(context.Background(), InstanceOptions{Limits: Limits{MaxSteps: 64}})
		if err != nil {
			t.Fatal(err)
		}
		defer instance.Close()
		execution, err := instance.Start("run")
		if err != nil {
			t.Fatal(err)
		}
		programIndex := 0
		var total int64
		for _, operation := range operations {
			if terminalExecutionState(execution.State()) {
				break
			}
			budget := int(operation%8) + 1
			_, executed, pollErr := execution.PollSteps(budget)
			if executed < 0 || executed > budget {
				t.Fatalf("PollSteps(%d) executed %d", budget, executed)
			}
			total += int64(executed)
			if pollErr != nil && execution.State() != ExecutionFailed && execution.State() != ExecutionCanceled {
				t.Fatalf("nonterminal poll error: state=%s err=%v", execution.State(), pollErr)
			}
			stats, statsErr := instance.RuntimeStats(context.Background())
			if statsErr != nil || stats.ExecutedSteps != total || stats.BlockedTasks < len(stats.BlockedContexts) || len(stats.BlockedContexts) > maxBlockedContexts {
				t.Fatalf("runtime stats = %#v, total=%d, err=%v", stats, total, statsErr)
			}
			if execution.State() == ExecutionRunning && operation&0x10 != 0 {
				programIndex = 1 - programIndex
				plan, prepareErr := instance.PreparePatch(context.Background(), programs[programIndex])
				if prepareErr != nil {
					t.Fatal(prepareErr)
				}
				if _, applyErr := instance.ApplyPatch(plan); applyErr != nil {
					t.Fatal(applyErr)
				}
			}
			if operation&0x20 != 0 {
				execution.Cancel()
				break
			}
		}
		if !terminalExecutionState(execution.State()) {
			execution.Cancel()
		}
		stats, err := instance.RuntimeStats(context.Background())
		if err != nil || stats.ActiveScopes != 0 || stats.PendingFFICalls != 0 || stats.Timers != 0 || stats.RetainedRevisions != 1 {
			t.Fatalf("runtime cleanup = %#v, %v", stats, err)
		}
	})
}
