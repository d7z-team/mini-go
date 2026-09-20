package runtime

import (
	"context"
)

func (machine *executionMachine) newScope(id, rootID int64, program bool, revision *instanceRevision) *executionScope {
	if machine.scopes == nil {
		machine.scopes = make(map[int64]*executionScope)
	}
	scope := &executionScope{
		id: id, rootID: rootID, program: program, budget: &executionBudget{wake: machine.vm.signalWake},
		rootState: ExecutionRunning, done: make(chan struct{}),
	}
	if revision != nil && revision.code != nil {
		scope.started = RevisionInfo{Generation: revision.generation, Hash: revision.code.image.Hash, SymbolsHash: revision.symbolsHash()}
	}
	machine.scopes[id] = scope
	return scope
}

func (machine *executionMachine) scope(id int64) *executionScope {
	if machine == nil || id == 0 {
		return nil
	}
	return machine.scopes[id]
}

func (machine *executionMachine) addTask(task *executionTask) {
	if task == nil || task.ownership.terminal() || (task.scope != nil && task.scope.settled) {
		return
	}
	if machine.tasks == nil {
		machine.tasks = make(map[int64]*executionTask)
	}
	if previous := machine.tasks[task.id]; previous != nil {
		panic("task identity already registered")
	}
	machine.tasks[task.id] = task
	if task.scope != nil {
		task.scope.tasks++
	}
}

func (machine *executionMachine) finishTask(task *executionTask, err error) {
	if task == nil || !task.ownership.terminate() {
		return
	}
	delete(machine.tasks, task.id)
	if task.scope != nil && !task.scope.settled && task.scope.tasks > 0 {
		task.scope.tasks--
	}
	if task.scope != nil && !task.scope.settled && err != nil && task.scope.err == nil {
		task.scope.err = err
	}
	machine.settleScope(task.scope)
}

func (machine *executionMachine) publishScopeRoot(scopeID int64, state ExecutionState, err error) {
	scope := machine.scope(scopeID)
	if scope == nil || scope.rootPublished {
		return
	}
	scope.rootPublished = true
	scope.rootState = state
	if err != nil {
		scope.err = err
	}
	machine.settleScope(scope)
}

func (machine *executionMachine) addScopeTimer(scope *executionScope) {
	if scope != nil && !scope.settled {
		scope.timers++
	}
}

func (machine *executionMachine) releaseScopeTimer(scope *executionScope) {
	if scope == nil || scope.settled {
		return
	}
	if scope.timers > 0 {
		scope.timers--
	}
	machine.settleScope(scope)
}

func (machine *executionMachine) addScopeFFI(scope *executionScope) {
	if scope != nil && !scope.settled {
		scope.ffiCalls++
	}
}

func (machine *executionMachine) releaseScopeFFI(scope *executionScope) {
	if scope == nil || scope.settled {
		return
	}
	if scope.ffiCalls > 0 {
		scope.ffiCalls--
	}
	machine.settleScope(scope)
}

func (machine *executionMachine) settleScope(scope *executionScope) {
	if machine == nil || scope == nil || scope.settled || scope.tasks != 0 || scope.timers != 0 || scope.ffiCalls != 0 {
		return
	}
	if !scope.rootPublished {
		return
	}
	scope.settled = true
	delete(machine.scopes, scope.id)
	stats := scope.snapshot()
	if scope.execution != nil {
		scope.execution.setScopeFinal(stats)
	}
	close(scope.done)
	if scope.execution != nil {
		scope.execution.notify()
	}
	if machine.vm != nil {
		machine.vm.sweepRetiredRevisions()
	}
}

func (machine *executionMachine) cancelScope(scope *executionScope, err error) {
	if machine == nil || scope == nil || scope.settled {
		return
	}
	if err == nil {
		err = context.Canceled
	}
	taskErr := err
	if scope.program && scope.rootPublished && scope.rootState == ExecutionCompleted && err == context.Canceled {
		taskErr = nil
	} else {
		scope.err = err
	}
	if !scope.rootPublished {
		scope.rootPublished = true
		if err == context.Canceled {
			scope.rootState = ExecutionCanceled
		} else {
			scope.rootState = ExecutionFailed
		}
	}
	for _, task := range machine.tasks {
		if task.scope == scope {
			machine.abortTask(task)
			machine.finishTask(task, taskErr)
		}
	}
	keepRunnable := machine.runnable[:0]
	for _, task := range machine.runnableTasks() {
		if task.scope != scope {
			keepRunnable = append(keepRunnable, task)
		}
	}
	clear(machine.runnable[len(keepRunnable):])
	machine.runnable = keepRunnable
	machine.runnableHead = 0
	keepBlocked := machine.blocked[:0]
	for _, task := range machine.blocked {
		if task.scope != scope {
			keepBlocked = append(keepBlocked, task)
		}
	}
	clear(machine.blocked[len(keepBlocked):])
	machine.blocked = keepBlocked
	if machine.paused != nil && machine.paused.scope == scope {
		machine.paused = nil
		if scope.execution != nil && scope.execution.instance != nil {
			scope.execution.instance.discardBackgroundPause()
		}
	}
	if machine.foreground == scope {
		machine.foreground = nil
	}
	if machine.vm != nil {
		machine.vm.cancelScopeResources(scope.id)
	}
	machine.settleScope(scope)
}

func (machine *executionMachine) cancelAllScopes(err error) {
	if machine == nil {
		return
	}
	scopes := make([]*executionScope, 0, len(machine.scopes))
	for _, scope := range machine.scopes {
		scopes = append(scopes, scope)
	}
	for _, scope := range scopes {
		machine.cancelScope(scope, err)
	}
}

func (machine *executionMachine) cancelRequestedScopes() *Execution {
	if machine == nil {
		return nil
	}
	scopes := make([]*executionScope, 0, len(machine.scopes))
	for _, scope := range machine.scopes {
		if scope != nil && scope.execution != nil && scope.execution.cancelRequested.Load() {
			scopes = append(scopes, scope)
		}
	}
	var foreground *Execution
	for _, scope := range scopes {
		if machine.foreground == scope {
			foreground = scope.execution
		}
		machine.cancelScope(scope, context.Canceled)
	}
	return foreground
}

func (scope *executionScope) snapshot() ScopeStats {
	if scope == nil {
		return ScopeStats{}
	}
	return ScopeStats{
		ID: scope.id, Started: scope.started, RootState: scope.rootState,
		Tasks: scope.tasks, Timers: scope.timers, FFICalls: scope.ffiCalls,
		Steps: scope.budget.steps.Load(), Done: scope.settled, Err: scope.err,
	}
}
