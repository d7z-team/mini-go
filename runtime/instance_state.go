package runtime

import (
	"context"
	"errors"
)

func (i *Instance) lifecycleState() instanceLifecycle {
	if i == nil {
		return instanceClosed
	}
	return instanceLifecycle(i.lifecycle.Load())
}

func (i *Instance) isOpen() bool { return i.lifecycleState() == instanceOpen }

func (i *Instance) markFaulted() {
	if i != nil {
		i.lifecycle.CompareAndSwap(uint32(instanceOpen), uint32(instanceFaulted))
	}
}

func (i *Instance) finish(err error) {
	if i == nil {
		return
	}
	if job := i.supervisor.Load(); job != nil {
		job.stop()
	}
	i.terminalMu.Lock()
	if i.terminalErr == nil {
		i.terminalErr = err
	}
	i.terminalMu.Unlock()
	i.doneOnce.Do(func() {
		if i.done != nil {
			close(i.done)
		}
	})
}

func (i *Instance) fail(err error) {
	if i == nil {
		return
	}
	i.markFaulted()
	i.finish(err)
}

// Done closes when the instance exits normally, fails, or is closed.
func (i *Instance) Done() <-chan struct{} {
	if i == nil || i.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return i.done
}

// Wait blocks until the instance reaches a terminal lifecycle state.
func (i *Instance) Wait(ctx context.Context) error {
	if i == nil {
		return errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-i.Done():
		return i.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Err reports the first fatal instance error after Done is closed.
func (i *Instance) Err() error {
	if i == nil {
		return errors.New("instance is closed")
	}
	i.terminalMu.RLock()
	defer i.terminalMu.RUnlock()
	return i.terminalErr
}

func (i *Instance) unavailableError() error {
	if i != nil && i.lifecycleState() == instanceFaulted {
		return ErrInstanceFaulted
	}
	return errors.New("instance is closed")
}

func (i *Instance) setBackgroundPause(execution *Execution) {
	if i == nil {
		return
	}
	i.debugMu.Lock()
	i.debugPaused = execution
	i.debugMu.Unlock()
}

func (i *Instance) clearBackgroundPause(execution *Execution) {
	if i == nil {
		return
	}
	i.debugMu.Lock()
	if i.debugPaused == execution {
		i.debugPaused = nil
	}
	i.debugMu.Unlock()
}

func (i *Instance) discardBackgroundPause() {
	if i == nil {
		return
	}
	i.debugMu.Lock()
	execution := i.debugPaused
	i.debugPaused = nil
	i.debugMu.Unlock()
	if execution != nil {
		execution.mu.Lock()
		execution.pause = nil
		execution.debugInspection = nil
		execution.mu.Unlock()
		execution.notify()
	}
}

func (i *Instance) isBackgroundPaused(execution *Execution) bool {
	if i == nil {
		return false
	}
	i.debugMu.RLock()
	defer i.debugMu.RUnlock()
	return i.debugPaused == execution
}

func (i *Instance) signalSupervisor() {
	if i == nil {
		return
	}
	if job := i.supervisor.Load(); job != nil {
		job.wake()
	}
}

func (i *Instance) supervise() bool {
	if !i.isOpen() || i.vm == nil {
		return false
	}
	if !i.driveMu.TryLock() {
		return false
	}
	defer i.driveMu.Unlock()
	if batch := i.batch; batch != nil {
		select {
		case <-batch.done:
		default:
			return false
		}
		i.supervisorRetry.Store(true)
		if err := i.vm.enterOwner(); err != nil {
			return false
		}
		i.supervisorRetry.Store(false)
		outcome := batch.machine.mergeTaskBatch(batch)
		i.batch = nil
		outcome = i.vm.finishPreparedOutcome(outcome)
		var paused *Execution
		if outcome.state == ExecutionPaused && i.vm.machine != nil && i.vm.machine.paused != nil {
			paused = i.vm.machine.paused.execution
		}
		i.vm.leaveOwner()
		switch outcome.state {
		case ExecutionPaused:
			if paused != nil {
				paused.captureBackgroundPause(outcome.pause)
			}
			return false
		case ExecutionFailed, ExecutionCanceled:
			i.fail(outcome.err)
			return false
		}
	}
	// Register before attempting ownership so release cannot race a failed
	// acquisition and leave this job asleep. Never park a pool worker on owner.
	i.supervisorRetry.Store(true)
	if err := i.vm.enterOwner(); err != nil {
		return false
	}
	i.supervisorRetry.Store(false)
	if !i.isOpen() || i.vm.machine == nil {
		i.vm.leaveOwner()
		return false
	}
	i.applyRequestedCancellationsLocked()
	if !i.isOpen() || i.active.Load() != nil || i.vm.machine == nil || i.vm.machine.paused != nil {
		i.vm.leaveOwner()
		return false
	}
	batch, outcome := i.vm.machine.prepareTaskBatch(i.parallelism, defaultPollQuantum)
	if batch != nil {
		batch.instance = i
		i.batch = batch
		i.vm.publishSlices(len(batch.runs))
		batch.launch()
		return false
	}
	outcome = i.vm.finishPreparedOutcome(outcome)
	var paused *Execution
	if outcome.state == ExecutionPaused && i.vm.machine != nil && i.vm.machine.paused != nil {
		paused = i.vm.machine.paused.execution
	}
	i.vm.sweepRetiredRevisions()
	i.vm.leaveOwner()
	switch outcome.state {
	case ExecutionPaused:
		if paused != nil {
			paused.captureBackgroundPause(outcome.pause)
		}
	case ExecutionFailed, ExecutionCanceled:
		i.fail(outcome.err)
	}
	return false
}

func (i *Instance) applyRequestedCancellationsLocked() {
	if i == nil || i.vm == nil || i.vm.machine == nil {
		return
	}
	foreground := i.vm.machine.cancelRequestedScopes()
	if foreground == nil {
		return
	}
	if foreground.program {
		i.vm.finishRun()
	} else {
		i.vm.finishForeground()
	}
	foreground.capture(failedRun(context.Canceled))
}

func (i *Instance) Close() error {
	return i.Shutdown(context.Background())
}

// Shutdown cancels active work and waits for pending calls to observe cancellation.
func (i *Instance) Shutdown(ctx context.Context) error {
	if i == nil || i.vm == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	i.beginShutdown()
	select {
	case <-i.shutdownDone:
		return i.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (i *Instance) beginShutdown() {
	if i == nil || i.vm == nil {
		return
	}
	i.shutdownOnce.Do(func() {
		i.lifecycle.Store(uint32(instanceClosing))
		i.vm.interruptRequested.Store(true)
		i.vm.signalWake()
		go func() {
			i.shutdownErr = i.shutdownInstance()
			i.lifecycle.Store(uint32(instanceClosed))
			i.finish(i.shutdownErr)
			close(i.shutdownDone)
		}()
	})
}

func (i *Instance) shutdownInstance() error {
	defer i.executor.unregister(i)
	i.driveMu.Lock()
	defer i.driveMu.Unlock()
	if err := i.vm.enterOwnerContext(context.Background()); err != nil {
		return err
	}
	if batch := i.batch; batch != nil {
		// Full ownership implies every published slice returned its roots and
		// result. Merge before aborting so no task is left detached from the
		// scheduler lifecycle.
		batch.machine.mergeTaskBatch(batch)
		i.batch = nil
	}
	i.patchMu.Lock()
	pendingPatch := i.pendingPatch
	i.patchMu.Unlock()
	i.vm.leaveOwner()
	if pendingPatch != nil {
		_ = pendingPatch.Close()
	}
	if err := i.vm.enterOwnerContext(context.Background()); err != nil {
		return err
	}
	if i.vm.machine != nil {
		i.vm.machine.abortTasks(context.Canceled)
	}
	if active := i.active.Swap(nil); active != nil {
		i.vm.finishRun()
		active.cancelOwned()
	} else {
		i.vm.finishRun()
	}
	i.vm.leaveOwner()
	var shutdownErr error
	if i.vm.ffiSession != nil {
		shutdownErr = i.vm.ffiSession.Shutdown(context.Background())
		i.vm.ffiSession = nil
	}
	if err := i.vm.enterOwnerContext(context.Background()); err != nil {
		return errors.Join(shutdownErr, err)
	}
	i.vm.debugger.mu.Lock()
	i.vm.debugger.closeLocked()
	i.vm.closeRevisions()
	i.vm.debugger.mu.Unlock()
	i.vm.instance = nil
	i.vm.leaveOwner()
	return shutdownErr
}
