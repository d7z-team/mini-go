package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionPoolSingleWorkerResumesWithoutWaitingForPeer(t *testing.T) {
	pool, err := newExecutionPool(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.shutdown(context.Background()) })
	done := make(chan struct{})
	var ready atomic.Bool
	var waiting *executionJob
	waiting = pool.job(func() bool {
		if ready.Load() {
			waiting.stop()
			close(done)
		}
		return false
	})
	producer := pool.job(func() bool {
		ready.Store(true)
		waiting.wake()
		return false
	})
	waiting.wake()
	producer.wake()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("single worker did not resume the waiting job")
	}
}

func TestExecutionPoolRetainsWakeUntilRunningSliceReturns(t *testing.T) {
	pool, err := newExecutionPool(2)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	var active atomic.Int32
	var overlap atomic.Bool
	job := pool.job(func() bool {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		if calls.Add(1) == 1 {
			close(entered)
			<-release // Test gate: notification deliberately precedes handoff.
		} else {
			close(done)
		}
		return false
	})
	t.Cleanup(func() {
		job.stop()
		_ = pool.shutdown(context.Background())
	})
	job.wake()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("job was not dispatched")
	}
	for range 100 {
		job.wake()
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notification was lost during ownership handoff")
	}
	job.stop()
	if err := pool.shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if overlap.Load() || calls.Load() != 2 {
		t.Fatalf("execution lease: overlap=%v slices=%d", overlap.Load(), calls.Load())
	}
}

func TestSupervisorBusyOwnerReleasesSingleWorkerUntilOwnershipReturns(t *testing.T) {
	pool, err := newExecutionPool(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.shutdown(context.Background()) })
	machine := &vm{ownerWake: make(chan struct{}, 1)}
	instance := &Instance{vm: machine}
	machine.instance = instance
	machine.owner.Store(true)
	var attempts atomic.Int32
	resumed := make(chan struct{})
	job := pool.job(func() bool {
		again := instance.supervise()
		if attempts.Add(1) == 2 {
			close(resumed)
		}
		return again
	})
	instance.supervisor.Store(job)
	job.wake()
	pool.job(func() bool {
		machine.leaveOwner()
		return false
	}).wake()
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("busy instance occupied the only worker or lost ownership notification")
	}
	if instance.supervisorRetry.Load() {
		t.Fatal("successful owner acquisition left a pending retry")
	}
}

func TestSynchronousCancelSettlesWithoutQueuingBackgroundOwnership(t *testing.T) {
	instance, err := patchTestProgram(t, patchCallArtifact(10, 1), "synchronous-cancel").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	pool, err := newExecutionPool(1)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(release)
		_ = pool.shutdown(context.Background())
	})
	pool.job(func() bool {
		close(entered)
		<-release
		return false
	}).wake()
	<-entered
	instance.supervisor.Load().stop()
	job := pool.job(instance.supervise)
	instance.supervisor.Store(job)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	execution.Cancel()
	if execution.State() != ExecutionCanceled {
		t.Fatal("synchronous cancellation depended on the occupied worker")
	}
	pool.mu.Lock()
	queued := job.queued
	pool.mu.Unlock()
	if queued {
		t.Fatal("completed cancellation left a background owner request racing the next host operation")
	}
}

func TestExecutorShutdownClosesAttachedInstancesAndRejectsNewOnes(t *testing.T) {
	executor, err := NewExecutor(1)
	if err != nil {
		t.Fatal(err)
	}
	program := patchTestProgram(t, patchCallArtifact(10, 1), "executor-shutdown")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-instance.Done():
	default:
		t.Fatal("executor returned before its instance cleanup completed")
	}
	if instance.lifecycleState() != instanceClosed {
		t.Fatalf("instance state after executor shutdown: %v", instance.lifecycleState())
	}
	if _, err := program.Instantiate(t.Context(), InstanceOptions{Executor: executor}); err == nil {
		t.Fatal("closed executor accepted a new instance")
	}
}

func TestOneWorkerExecutorAdvancesMultipleInstancesWithoutPinnedSupervisors(t *testing.T) {
	executor, err := NewExecutor(1)
	if err != nil {
		t.Fatal(err)
	}
	program := patchTestProgram(t, publicParallelArtifact(), "shared-one-worker")
	instances := make([]*Instance, 2)
	executions := make([]*Execution, 2)
	for index := range instances {
		instances[index], err = program.Instantiate(t.Context(), InstanceOptions{
			Parallelism: 4,
			Executor:    executor,
		})
		if err != nil {
			_ = executor.Shutdown(context.Background())
			t.Fatal(err)
		}
		executions[index], err = instances[index].Start("run")
		if err != nil {
			_ = executor.Shutdown(context.Background())
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var group sync.WaitGroup
	errorsByInstance := make([]error, len(executions))
	for index, execution := range executions {
		group.Go(func() {
			if _, waitErr := execution.Wait(ctx); waitErr != nil {
				errorsByInstance[index] = waitErr
				return
			}
			_, errorsByInstance[index] = execution.WaitScope(ctx)
		})
	}
	group.Wait()
	for _, waitErr := range errorsByInstance {
		if waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	if err := executor.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
