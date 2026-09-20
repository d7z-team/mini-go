package runtime

import "sync"

// taskOwnership protects dispatch and wait handoffs, not guest execution.
// Only the holder of the running lease may access a task's continuation.
type taskOwnership struct {
	mu    sync.Mutex
	state taskDispatchState
	wait  *taskWaitTicket
}

type taskDispatchState uint8

const (
	taskDetached taskDispatchState = iota
	taskQueued
	taskRunning
	taskParking
	taskWaiting
	taskTerminal
)

// Identity, rather than a wrapping counter, distinguishes successive waits.
// A delayed notification owns its ticket until it has been delivered.
type taskWaitTicket struct {
	notified bool // protected by the task's ownership mutex
}

func (ownership *taskOwnership) enqueue() bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.state != taskDetached {
		return false
	}
	ownership.state = taskQueued
	return true
}

func (ownership *taskOwnership) acquire() bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.state != taskQueued {
		return false
	}
	ownership.state = taskRunning
	return true
}

func (ownership *taskOwnership) beginWait() *taskWaitTicket {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.state != taskRunning {
		panic("task wait requires an execution lease")
	}
	ownership.wait = &taskWaitTicket{}
	ownership.state = taskParking
	return ownership.wait
}

// relinquish publishes the continuation before control can resume it. An
// early notification never grants another worker access to a parking task.
func (ownership *taskOwnership) relinquish() {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	switch ownership.state {
	case taskRunning:
		ownership.state = taskDetached
	case taskParking:
		ownership.state = taskWaiting
	default:
		panic("task relinquished without an execution lease")
	}
}

func (ownership *taskOwnership) notify(ticket *taskWaitTicket) bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ticket == nil || ownership.wait != ticket || (ownership.state != taskParking && ownership.state != taskWaiting) {
		return false
	}
	ticket.notified = true
	return true
}

func (ownership *taskOwnership) resume(ticket *taskWaitTicket) bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ticket == nil || ownership.wait != ticket || ownership.state != taskWaiting {
		return false
	}
	ownership.wait = nil
	ownership.state = taskDetached
	return true
}

func (ownership *taskOwnership) notified(ticket *taskWaitTicket) bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	return ticket != nil && ownership.wait == ticket && ticket.notified && ownership.state == taskWaiting
}

func (ownership *taskOwnership) terminal() bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	return ownership.state == taskTerminal
}

func (ownership *taskOwnership) terminate() bool {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.state == taskTerminal {
		return false
	}
	if ownership.state == taskRunning || ownership.state == taskParking {
		panic("task terminated before execution lease returned")
	}
	ownership.wait = nil
	ownership.state = taskTerminal
	return true
}
