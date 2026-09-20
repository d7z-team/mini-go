package runtime

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func TestControlRequestWaitsForEveryTaskLease(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			vm := &vm{ownerWake: make(chan struct{}, 1)}
			for range workers {
				if !vm.acquireSlice() {
					t.Fatal("dispatch denied before stop request")
				}
			}
			request := vm.requestControl()
			defer request.cancel()
			if vm.acquireSlice() || request.acquire() {
				t.Fatal("stop request admitted execution or inspected active roots")
			}
			if err := vm.enterOwner(); !errors.Is(err, ErrBusy) {
				t.Fatalf("nonblocking control entered an active instance: %v", err)
			}
			for index := range workers {
				vm.releaseSlice()
				if index != workers-1 {
					if request.acquire() {
						t.Fatal("control entered before the last acknowledgement")
					}
					select {
					case <-vm.ownerWake:
						t.Fatal("partial acknowledgement advertised stopped roots")
					default:
					}
				}
			}
			select {
			case <-vm.ownerWake:
			default:
				t.Fatal("last acknowledgement did not wake control")
			}
			if !request.acquire() || vm.acquireSlice() {
				t.Fatal("stopped control and execution ownership overlap")
			}
			vm.leaveOwner()
			if !vm.acquireSlice() {
				t.Fatal("completed control kept dispatch stopped")
			}
			vm.releaseSlice()
		})
	}
}

func TestCanceledSafepointPreservesOtherRequests(t *testing.T) {
	vm := &vm{ownerWake: make(chan struct{}, 1)}
	first, second := vm.requestControl(), vm.requestControl()
	first.cancel()
	first.cancel()
	if vm.acquireSlice() || !second.acquire() {
		t.Fatal("canceling one requester released another request's stop")
	}
	vm.leaveOwner()
	if !vm.acquireSlice() {
		t.Fatal("completed requests retained dispatch stop")
	}
	vm.releaseSlice()
}

func TestSingleWorkerReturnsLeaseBeforeRunningSafepointControl(t *testing.T) {
	pool, err := newExecutionPool(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.shutdown(context.Background()) })
	vm := &vm{ownerWake: make(chan struct{}, 1)}
	instance := &Instance{vm: vm}
	vm.instance = instance
	if !vm.acquireSlice() {
		t.Fatal("could not dispatch task")
	}
	request := vm.requestControl()
	defer request.cancel()
	completed := make(chan struct{})
	control := pool.job(func() bool {
		instance.supervisorRetry.Store(true)
		if !request.acquire() {
			return false
		}
		instance.supervisorRetry.Store(false)
		vm.leaveOwner()
		close(completed)
		return false
	})
	instance.supervisor.Store(control)
	control.wake()
	pool.job(func() bool {
		vm.releaseSlice()
		return false
	}).wake()
	select {
	case <-completed:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

func TestSafepointPublishesPrivateRootsBeforeControlInspection(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			pool, err := newExecutionPool(workers)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pool.shutdown(context.Background()) })
			vm := &vm{ownerWake: make(chan struct{}, 1)}
			// Each slot models roots exclusively owned by a dispatched task.
			// Deliberately ordinary memory: the lease gate must publish writes.
			roots := make([]int, workers)
			var ready sync.WaitGroup
			ready.Add(workers)
			var started sync.WaitGroup
			started.Add(workers)
			var finished atomic.Bool
			for index := range workers {
				published := false
				pool.job(func() bool {
					if finished.Load() {
						ready.Done()
						return false
					}
					if vm.acquireSlice() {
						roots[index]++
						vm.releaseSlice()
						if !published {
							published = true
							started.Done()
						}
					}
					return true
				}).wake()
			}
			started.Wait()
			for range 100 {
				if err := vm.enterOwnerContext(t.Context()); err != nil {
					t.Fatal(err)
				}
				for index := range roots {
					roots[index] = 0
				}
				if vm.acquireSlice() {
					t.Fatal("inspection permitted mutation of private roots")
				}
				vm.leaveOwner()
			}
			finished.Store(true)
			ready.Wait()
		})
	}
}
