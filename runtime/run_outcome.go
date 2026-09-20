package runtime

import (
	"context"
	"errors"
)

// runOutcome separates cooperative scheduler stops from execution failures.
type runOutcome struct {
	executed int
	state    ExecutionState
	result   vmResult
	err      error
	pause    *debugEvent
}

func failedRun(err error) runOutcome {
	state := ExecutionFailed
	if errors.Is(err, context.Canceled) {
		state = ExecutionCanceled
	}
	return runOutcome{state: state, err: err}
}

func (r runOutcome) suspended() bool {
	return r.state == ExecutionRunning || r.state == ExecutionPending || r.state == ExecutionPaused
}
