package runtime

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestTaskFragmentsShareFrameStorageAcrossExecutorWorkers(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			artifact := patchCallArtifact(10, 1)
			entry := &artifact.Functions[0]
			entry.Instructions = entry.Instructions[:1]
			for range 128 {
				entry.Instructions = append(entry.Instructions,
					ir.Instruction{Op: string(ir.OpCallDirect), Payload: testPayload(ir.CallPayload{Function: "fn.value", ResultCount: 1})},
					ir.Instruction{Op: string(ir.OpBinary), Payload: testPayload(ir.OperatorPayload{Operator: "+"})},
				)
			}
			entry.Instructions = append(entry.Instructions, ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{ResultCount: 1})})
			vm, err := loadTestEngine(artifact)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(vm.closeRevisions)
			machine := &executionMachine{vm: vm}
			vm.machine = machine
			module := vm.rootModule()
			module.state.initState = moduleReady
			pool, err := newExecutionPool(workers)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pool.shutdown(context.Background()) })
			const taskCount = 8
			tasks := make([]*executionTask, taskCount)
			for index := range tasks {
				id := int64(index + 1)
				frame, err := vm.newExecutionFrame(module, "fn.entry", nil, nil, id, -1, false)
				if err != nil {
					t.Fatal(err)
				}
				scope := machine.newScope(id, id, false, vm.revision.Load())
				tasks[index] = &executionTask{id: id, scope: scope, budget: scope.budget, frames: []*executionFrame{frame}}
				machine.addTask(tasks[index])
				tasks[index].ownership.enqueue()
			}
			var completed sync.WaitGroup
			completed.Add(taskCount)
			failures := make([]error, taskCount)
			results := make([][]vmValue, taskCount)
			steps := make([]int, taskCount)
			for index, task := range tasks {
				pool.job(func() bool {
					if !vm.acquireSlice() {
						return true
					}
					defer vm.releaseSlice()
					if !task.ownership.acquire() {
						failures[index] = fmt.Errorf("task %d lost its lease", task.id)
						completed.Done()
						return false
					}
					slice := taskSlice{limit: taskInstructionQuantum}
					yield, values, err := machine.runTask(task, &slice)
					task.ownership.relinquish()
					steps[index] += slice.executed
					if err == nil && (yield.kind == taskYieldPoll || yield.kind == taskYieldCooperate) {
						task.quantumSteps = 0
						task.ownership.enqueue()
						return true
					}
					if err == nil && yield.kind != taskYieldComplete {
						err = fmt.Errorf("unexpected task yield %q", yield.kind)
					}
					failures[index], results[index] = err, values
					completed.Done()
					return false
				}).wake()
			}
			completed.Wait()
			if err := pool.shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			for index, task := range tasks {
				machine.abortTask(task)
				machine.finishTask(task, failures[index])
				if failures[index] != nil {
					t.Fatal(failures[index])
				}
				if len(results[index]) != 1 {
					t.Fatalf("task %d results: %v", task.id, results[index])
				}
				value, err := asInt64(results[index][0])
				if err != nil || value != 138 || steps[index] != 514 {
					t.Fatalf("task %d: value=%d steps=%d err=%v", task.id, value, steps[index], err)
				}
			}
		})
	}
}

func publicParallelArtifact() ir.Artifact {
	artifact := ir.NewArtifact("scheduler/public-parallel", "main")
	entry := make([]ir.Instruction, 0, 9)
	for range 4 {
		entry = append(entry,
			ir.Instruction{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.child"})},
			ir.Instruction{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
		)
	}
	entry = append(entry, ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})})
	child := make([]ir.Instruction, 0, taskInstructionQuantum*32+1)
	for range taskInstructionQuantum * 16 {
		child = append(child,
			ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			ir.Instruction{Op: string(ir.OpPop)},
		)
	}
	child = append(child, ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})})
	artifact.Functions = []ir.Function{
		{ID: "fn.entry", Signature: testSignature("function() Void"), Instructions: entry},
		{ID: "fn.child", RevisionLocal: true, Signature: testSignature("function() Void"), Instructions: child},
	}
	return artifact
}

func TestPublicInstanceDispatchesSameInstanceTasksInParallel(t *testing.T) {
	artifact := publicParallelArtifact()
	for _, parallelism := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(parallelism), func(t *testing.T) {
			executor, err := NewExecutor(parallelism)
			if err != nil {
				t.Fatal(err)
			}
			var rootID atomic.Int64
			var active atomic.Int64
			var maximum atomic.Int64
			release := make(chan struct{})
			var releaseOnce sync.Once
			observer := func(taskID int64, entering bool, batch []int64) {
				if taskID == rootID.Load() {
					return
				}
				if entering {
					current := active.Add(1)
					updateAtomicMaximum(&maximum, current)
					allBackground := len(batch) >= parallelism
					for _, peer := range batch {
						allBackground = allBackground && peer != rootID.Load()
					}
					if parallelism > 1 && allBackground {
						if current >= int64(parallelism) {
							releaseOnce.Do(func() { close(release) })
						}
						<-release
					}
				} else {
					active.Add(-1)
				}
			}
			instance, err := patchTestProgram(t, artifact, "public-parallel-"+strconv.Itoa(parallelism)).Instantiate(t.Context(), InstanceOptions{
				Parallelism:  parallelism,
				Executor:     executor,
				taskObserver: observer,
			})
			if err != nil {
				_ = executor.Shutdown(context.Background())
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = instance.Close()
				_ = executor.Shutdown(context.Background())
			})
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			rootID.Store(instance.vm.machine.foreground.rootID)
			if _, err := execution.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-execution.scopeDone:
			case <-time.After(5 * time.Second):
				t.Fatal("spawned tasks did not settle")
			}
			if parallelism == 1 && maximum.Load() != 1 {
				t.Fatalf("single-worker instance reached %d concurrent task slices", maximum.Load())
			}
			if maximum.Load() != int64(parallelism) {
				t.Fatalf("instance parallelism %d admitted %d task slices", parallelism, maximum.Load())
			}
		})
	}
}
