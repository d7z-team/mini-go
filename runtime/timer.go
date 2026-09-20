package runtime

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/d7z-team/mini-go/compiler/types"
)

type runtimeTimer struct {
	mu             sync.Mutex
	vm             *vm
	module         *moduleInstance
	signal         vmValue
	resource       *waitableResource
	alarm          ClockTimer
	deadline       time.Time
	period         time.Duration
	scope          *executionScope
	pinnedRevision *instanceRevision
	ready          bool
	stopped        bool
}

func (timer *runtimeTimer) markReady() {
	timer.mu.Lock()
	wake := !timer.stopped
	if !timer.stopped {
		timer.ready = true
	}
	timer.mu.Unlock()
	if wake {
		timer.vm.signalWake()
	}
}

func (vm *vm) startTimer(scope *executionScope, module *moduleInstance, signal vmValue, delay, period time.Duration) error {
	if vm == nil || module == nil || vm.machine == nil {
		return errors.New("timer requires an active execution")
	}
	if scope == nil || scope.settled {
		return errors.New("timer requires an active execution scope")
	}
	resource, err := waitableValueData(module, signal)
	if err != nil {
		return err
	}
	if resource == nil || !resource.ElemType.Primitive(types.PrimitiveBool) || resource.Capacity != 1 {
		return errors.New("timer signal must be a non-nil buffered chan bool with capacity 1")
	}
	if period < 0 {
		return errors.New("timer period must not be negative")
	}
	if vm.timers == nil {
		vm.timers = make(map[*waitableResource]*runtimeTimer)
	}
	if _, exists := vm.timers[resource]; exists {
		return errors.New("timer signal is already active")
	}
	if err := vm.reservePendingEvent(); err != nil {
		return err
	}
	if delay < 0 {
		delay = 0
	}
	var pinnedRevision *instanceRevision
	if module.revision.retain() {
		pinnedRevision = module.revision
	}
	timer := &runtimeTimer{
		vm: vm, module: module, signal: signal, resource: resource,
		deadline: vm.clock.Now().Add(delay), period: period, scope: scope,
		pinnedRevision: pinnedRevision,
	}
	vm.timers[resource] = timer
	vm.machine.addScopeTimer(scope)
	if delay == 0 {
		timer.ready = true
		vm.signalWake()
		return nil
	}
	timer.alarm = vm.clock.AfterFunc(delay, timer.markReady)
	if timer.alarm == nil {
		vm.removeTimer(resource, timer)
		return errors.New("clock returned a nil timer")
	}
	return nil
}

func (vm *vm) stopTimer(module *moduleInstance, signal vmValue) (bool, error) {
	resource, err := waitableValueData(module, signal)
	if err != nil {
		return false, err
	}
	timer := vm.timers[resource]
	if timer == nil {
		return false, nil
	}
	timer.mu.Lock()
	if timer.stopped {
		timer.mu.Unlock()
		return false, nil
	}
	timer.stopped = true
	alarm := timer.alarm
	timer.mu.Unlock()
	if alarm != nil {
		alarm.Stop()
	}
	var closeErr error
	if !resource.Closed {
		closeErr = waitableCloseValue(module, signal)
	}
	vm.removeTimer(resource, timer)
	return true, closeErr
}

func (vm *vm) drainReadyTimers() error {
	for resource, timer := range vm.timers {
		timer.mu.Lock()
		if timer.stopped || !timer.ready {
			timer.mu.Unlock()
			continue
		}
		timer.ready = false
		oneShot := timer.period == 0
		if oneShot {
			timer.stopped = true
		}
		deadline, period := timer.deadline, timer.period
		timer.mu.Unlock()

		if _, err := waitableTrySendValue(timer.module, timer.signal, newVMValue("Bool", true)); err != nil {
			vm.discardTimer(resource, timer)
			return fmt.Errorf("deliver timer signal: %w", err)
		}
		if oneShot {
			vm.removeTimer(resource, timer)
			continue
		}

		now := vm.clock.Now()
		next := deadline.Add(period)
		if !next.After(now) {
			elapsed := now.Sub(deadline)
			next = now.Add(period - elapsed%period)
		}
		timer.mu.Lock()
		if timer.stopped {
			timer.mu.Unlock()
			continue
		}
		timer.deadline = next
		timer.mu.Unlock()
		alarm := vm.clock.AfterFunc(next.Sub(now), timer.markReady)
		if alarm == nil {
			vm.discardTimer(resource, timer)
			return errors.New("clock returned a nil timer")
		}
		timer.mu.Lock()
		if timer.stopped {
			timer.mu.Unlock()
			alarm.Stop()
			continue
		}
		timer.alarm = alarm
		timer.mu.Unlock()
	}
	return nil
}

func (vm *vm) discardTimer(resource *waitableResource, timer *runtimeTimer) {
	if timer == nil {
		return
	}
	timer.mu.Lock()
	wasStopped := timer.stopped
	timer.stopped = true
	alarm := timer.alarm
	timer.mu.Unlock()
	if !wasStopped && alarm != nil {
		alarm.Stop()
	}
	vm.removeTimer(resource, timer)
}

func (vm *vm) removeTimer(resource *waitableResource, timer *runtimeTimer) {
	if vm == nil || timer == nil || vm.timers[resource] != timer {
		return
	}
	delete(vm.timers, resource)
	vm.releasePendingEvent(0)
	if vm.machine != nil {
		vm.machine.releaseScopeTimer(timer.scope)
	}
	if timer.pinnedRevision != nil {
		timer.pinnedRevision.release()
		timer.pinnedRevision = nil
	}
	timer.mu.Lock()
	timer.stopped = true
	timer.vm = nil
	timer.module = nil
	timer.signal = vmValue{}
	timer.resource = nil
	timer.alarm = nil
	timer.scope = nil
	timer.mu.Unlock()
}

func (vm *vm) closeTimers() {
	for resource, timer := range vm.timers {
		vm.discardTimer(resource, timer)
	}
	clear(vm.timers)
}

func (vm *vm) cancelScopeTimers(scopeID int64) {
	for resource, timer := range vm.timers {
		if timer != nil && timer.scope != nil && timer.scope.id == scopeID {
			vm.discardTimer(resource, timer)
		}
	}
}
