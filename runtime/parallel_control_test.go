package runtime

import (
	"context"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type productionSliceGate struct {
	root    atomic.Int64
	active  atomic.Int32
	opened  atomic.Bool
	counted sync.Map
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newProductionSliceGate() *productionSliceGate {
	return &productionSliceGate{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (gate *productionSliceGate) observe(taskID int64, entering bool, batch []int64) {
	gate.root.CompareAndSwap(0, taskID)
	if taskID == gate.root.Load() {
		return
	}
	if !entering {
		if _, counted := gate.counted.LoadAndDelete(taskID); counted {
			gate.active.Add(-1)
		}
		return
	}
	if gate.opened.Load() || len(batch) < 2 {
		return
	}
	for _, peer := range batch {
		if peer == gate.root.Load() {
			return
		}
	}
	if gate.opened.Load() {
		return
	}
	if _, counted := gate.counted.LoadOrStore(taskID, struct{}{}); counted {
		return
	}
	gate.active.Add(1)
	gate.entered <- struct{}{}
	<-gate.release
}

func (gate *productionSliceGate) open() {
	gate.once.Do(func() {
		gate.opened.Store(true)
		close(gate.release)
	})
}

func TestProductionSlicesUseOneBoundaryForStatsPatchAndRevisionInspection(t *testing.T) {
	for _, operation := range []string{"stats", "patch", "revision-roots"} {
		t.Run(operation, func(t *testing.T) {
			executor, err := NewExecutor(2)
			if err != nil {
				t.Fatal(err)
			}
			gate := newProductionSliceGate()
			base := patchTestProgram(t, publicParallelArtifact(), "parallel-control-base-"+operation)
			instance, err := base.Instantiate(t.Context(), InstanceOptions{
				Parallelism:  2,
				Executor:     executor,
				taskObserver: gate.observe,
			})
			if err != nil {
				_ = executor.Shutdown(context.Background())
				t.Fatal(err)
			}
			t.Cleanup(func() {
				gate.open()
				_ = instance.Close()
				_ = executor.Shutdown(context.Background())
			})

			var plan *PatchPlan
			if operation == "patch" {
				targetArtifact := publicParallelArtifact()
				child := &targetArtifact.Functions[1]
				last := child.Instructions[len(child.Instructions)-1]
				child.Instructions = append(child.Instructions[:len(child.Instructions)-1],
					ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
					ir.Instruction{Op: string(ir.OpPop)},
					last,
				)
				plan, err = instance.PreparePatch(t.Context(), patchTestProgram(t, targetArtifact, "parallel-control-target"))
				if err != nil {
					t.Fatal(err)
				}
			}

			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() {
				_, err := execution.Wait(t.Context())
				waited <- err
			}()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for range 2 {
				select {
				case <-gate.entered:
				case <-ctx.Done():
					t.Fatalf("production task slices did not overlap: %v", ctx.Err())
				}
			}
			if gate.active.Load() != 2 {
				t.Fatalf("active task slices = %d, want 2", gate.active.Load())
			}

			controlled := make(chan error, 1)
			go func() {
				var controlErr error
				switch operation {
				case "stats":
					_, controlErr = instance.RuntimeStats(ctx)
				case "patch":
					_, controlErr = instance.ApplyPatch(plan)
				case "revision-roots":
					_, controlErr = instance.RevisionRoots(ctx, 1, RevisionRootLimits{})
				}
				controlled <- controlErr
			}()
			for instance.vm.controlWaiters.Load() == 0 {
				select {
				case err := <-controlled:
					t.Fatalf("%s crossed active task slices: %v", operation, err)
				case <-ctx.Done():
					t.Fatalf("%s did not publish its control request: %v", operation, ctx.Err())
				default:
					goruntime.Gosched()
				}
			}
			select {
			case err := <-controlled:
				t.Fatalf("%s completed before task roots returned: %v", operation, err)
			default:
			}
			gate.open()
			select {
			case err := <-controlled:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatalf("%s did not complete after the task boundary: %v", operation, ctx.Err())
			}
			if operation == "patch" && instance.Revision().Generation != 2 {
				t.Fatalf("patch revision = %d, want 2", instance.Revision().Generation)
			}
			select {
			case err := <-waited:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatalf("root execution did not finish: %v", ctx.Err())
			}
			if _, err := execution.WaitScope(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDebugPausePublishesOnlyAfterEveryProductionSliceStops(t *testing.T) {
	executor, err := NewExecutor(2)
	if err != nil {
		t.Fatal(err)
	}
	gate := newProductionSliceGate()
	instance, err := patchTestProgram(t, publicParallelArtifact(), "parallel-debug-pause").Instantiate(t.Context(), InstanceOptions{
		Parallelism:  2,
		Executor:     executor,
		taskObserver: gate.observe,
	})
	if err != nil {
		_ = executor.Shutdown(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gate.open()
		_ = instance.Close()
		_ = executor.Shutdown(context.Background())
	})
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := execution.Wait(t.Context())
		waited <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatalf("production task slices did not overlap: %v", ctx.Err())
		}
	}
	if err := execution.RequestPause(); err != nil {
		t.Fatal(err)
	}
	if execution.State() == ExecutionPaused {
		t.Fatal("debug pause became visible while task slices were active")
	}
	if _, ok := execution.PauseEvent(); ok {
		t.Fatal("debug event exposed private task state before the all-stop boundary")
	}
	gate.open()
	select {
	case err := <-waited:
		if err == nil || err.Error() != "execution is paused" {
			t.Fatalf("pause result = %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("debug pause did not complete after the task boundary: %v", ctx.Err())
	}
	if _, err := execution.DebugSnapshot(); err != nil {
		t.Fatal(err)
	}
}
