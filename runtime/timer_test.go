package runtime

import (
	"sync/atomic"
	"testing"
	"time"
)

type lateAlarmClock struct {
	callback func()
	stops    atomic.Int32
}

func (*lateAlarmClock) Now() time.Time { return time.Unix(100, 0) }
func (clock *lateAlarmClock) AfterFunc(_ time.Duration, callback func()) ClockTimer {
	clock.callback = callback
	return clock
}
func (clock *lateAlarmClock) Stop() bool { clock.stops.Add(1); return false }

func TestRuntimeTimerLateCallbackCannotReviveStoppedTimer(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		clock := &lateAlarmClock{}
		machine, module := newTimerTestVM(clock)
		revision := &instanceRevision{}
		module.revision = revision
		signal := newTimerSignal(t, module)
		if err := machine.startTimer(machine.machine.scope(1), module, signal, time.Hour, 0); err != nil {
			t.Fatal(err)
		}
		ready, done := make(chan struct{}), make(chan struct{})
		go func() { <-ready; clock.callback(); close(done) }()
		if concurrent {
			close(ready)
		}
		_, stopErr := machine.stopTimer(module, signal)
		if !concurrent {
			close(ready)
		}
		<-done
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		if err := machine.drainReadyTimers(); err != nil {
			t.Fatal(err)
		}
		if len(machine.timers) != 0 || machine.pendingEvents.Load() != 0 || revision.pins != 0 || clock.stops.Load() != 1 {
			t.Fatal("late callback revived timer ownership")
		}
		if fired, closed := receiveTimerSignal(t, module, signal); fired || !closed {
			t.Fatal("stopped timer delivered a signal")
		}
	}
}

type nilAlarmClock struct{ now time.Time }

func (clock nilAlarmClock) Now() time.Time { return clock.now }

func (nilAlarmClock) AfterFunc(time.Duration, func()) ClockTimer { return nil }

func newTimerTestVM(clock Clock) (*vm, *moduleInstance) {
	machine := &vm{
		clock: clock, timers: make(map[*waitableResource]*runtimeTimer), wake: make(chan struct{}, 1),
		limits: normalizeLimits(Limits{}),
	}
	machine.machine = &executionMachine{vm: machine}
	machine.machine.newScope(1, 1, false, nil)
	return machine, &moduleInstance{vm: machine}
}

func newTimerSignal(t *testing.T, module *moduleInstance) vmValue {
	t.Helper()
	signal, err := makeWaitableValue(module, "Waitable<Bool>", newVMValue("Int", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	return signal
}

func receiveTimerSignal(t *testing.T, module *moduleInstance, signal vmValue) (bool, bool) {
	t.Helper()
	value, received, closed, err := waitableTryRecvValue(module, signal)
	if err != nil {
		t.Fatal(err)
	}
	if !received {
		return false, closed
	}
	fired, ok := value.Data.(bool)
	if !ok {
		t.Fatalf("timer signal = %#v", value)
	}
	return fired, closed
}

func TestRuntimeTimersWakeSameDeadlineTogether(t *testing.T) {
	clock := NewManualClock(time.Unix(100, 0))
	machine, module := newTimerTestVM(clock)
	first, second := newTimerSignal(t, module), newTimerSignal(t, module)
	if err := machine.startTimer(machine.machine.scope(1), module, first, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if err := machine.startTimer(machine.machine.scope(1), module, second, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if got := machine.pendingEvents.Load(); got != 2 {
		t.Fatalf("pending timers = %d", got)
	}
	if err := clock.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := machine.drainReadyTimers(); err != nil {
		t.Fatal(err)
	}
	for index, signal := range []vmValue{first, second} {
		if fired, _ := receiveTimerSignal(t, module, signal); !fired {
			t.Fatalf("timer %d did not fire", index)
		}
	}
	if got := machine.pendingEvents.Load(); got != 0 || len(machine.timers) != 0 {
		t.Fatalf("completed timers retained: pending=%d active=%d", got, len(machine.timers))
	}
}

func TestRuntimeTimerStopClosesSignalAndReleasesEvent(t *testing.T) {
	clock := NewManualClock(time.Unix(200, 0))
	machine, module := newTimerTestVM(clock)
	signal := newTimerSignal(t, module)
	if err := machine.startTimer(machine.machine.scope(1), module, signal, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	stopped, err := machine.stopTimer(module, signal)
	if err != nil || !stopped {
		t.Fatalf("Stop = %t, %v", stopped, err)
	}
	stopped, err = machine.stopTimer(module, signal)
	if err != nil || stopped {
		t.Fatalf("second Stop = %t, %v", stopped, err)
	}
	if _, closed := receiveTimerSignal(t, module, signal); !closed {
		t.Fatal("stopped timer signal remained open")
	}
	if got := machine.pendingEvents.Load(); got != 0 || len(machine.timers) != 0 {
		t.Fatalf("stopped timer retained: pending=%d active=%d", got, len(machine.timers))
	}
}

func TestRuntimeTimerPinsItsCreatingRevision(t *testing.T) {
	clock := NewManualClock(time.Unix(250, 0))
	machine, module := newTimerTestVM(clock)
	revision := &instanceRevision{}
	module.revision = revision
	signal := newTimerSignal(t, module)
	if err := machine.startTimer(machine.machine.scope(1), module, signal, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if revision.pins != 1 {
		t.Fatalf("timer revision pins = %d", revision.pins)
	}
	if stopped, err := machine.stopTimer(module, signal); err != nil || !stopped {
		t.Fatalf("stop timer = %t, %v", stopped, err)
	}
	if revision.pins != 0 {
		t.Fatalf("released timer revision pins = %d", revision.pins)
	}
}

func TestRuntimeTickerSkipsElapsedPeriods(t *testing.T) {
	clock := NewManualClock(time.Unix(300, 0))
	machine, module := newTimerTestVM(clock)
	signal := newTimerSignal(t, module)
	if err := machine.startTimer(machine.machine.scope(1), module, signal, time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := clock.Advance(3500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := machine.drainReadyTimers(); err != nil {
		t.Fatal(err)
	}
	if fired, _ := receiveTimerSignal(t, module, signal); !fired {
		t.Fatal("ticker did not deliver its first available tick")
	}
	if err := clock.Advance(499 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := machine.drainReadyTimers(); err != nil {
		t.Fatal(err)
	}
	if fired, _ := receiveTimerSignal(t, module, signal); fired {
		t.Fatal("ticker fired before its fixed next deadline")
	}
	if err := clock.Advance(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := machine.drainReadyTimers(); err != nil {
		t.Fatal(err)
	}
	if fired, _ := receiveTimerSignal(t, module, signal); !fired {
		t.Fatal("ticker did not preserve its fixed deadline")
	}
	machine.closeTimers()
	if got := machine.pendingEvents.Load(); got != 0 {
		t.Fatalf("ticker cleanup retained %d pending events", got)
	}
}

func TestRuntimeTimerCleanupStopsEveryAlarmOnce(t *testing.T) {
	clock := NewManualClock(time.Unix(400, 0))
	machine, module := newTimerTestVM(clock)
	for index := 0; index < 3; index++ {
		if err := machine.startTimer(machine.machine.scope(1), module, newTimerSignal(t, module), time.Hour, 0); err != nil {
			t.Fatal(err)
		}
	}
	machine.closeTimers()
	machine.closeTimers()
	if got := machine.pendingEvents.Load(); got != 0 || len(machine.timers) != 0 {
		t.Fatalf("timer cleanup retained state: pending=%d active=%d", got, len(machine.timers))
	}
}

func TestRuntimeTimerStartFailureReleasesPendingEvent(t *testing.T) {
	machine, module := newTimerTestVM(nilAlarmClock{now: time.Unix(500, 0)})
	if err := machine.startTimer(machine.machine.scope(1), module, newTimerSignal(t, module), time.Second, 0); err == nil {
		t.Fatal("timer start accepted a nil clock alarm")
	}
	if got := machine.pendingEvents.Load(); got != 0 || len(machine.timers) != 0 {
		t.Fatalf("failed timer retained state: pending=%d active=%d", got, len(machine.timers))
	}
}

func TestCancelScopeTimersKeepsOtherScopes(t *testing.T) {
	clock := NewManualClock(time.Unix(600, 0))
	machine, module := newTimerTestVM(clock)
	first := newTimerSignal(t, module)
	firstScope := machine.machine.newScope(11, 11, false, nil)
	if err := machine.startTimer(firstScope, module, first, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	second := newTimerSignal(t, module)
	secondScope := machine.machine.newScope(12, 12, false, nil)
	if err := machine.startTimer(secondScope, module, second, time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	machine.cancelScopeTimers(11)
	secondResource, err := waitableValueData(module, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(machine.timers) != 1 || machine.timers[secondResource] == nil {
		t.Fatalf("timers after scope cancel = %#v", machine.timers)
	}
	if got := machine.pendingEvents.Load(); got != 1 {
		t.Fatalf("pending events after scope cancel = %d", got)
	}
	machine.closeTimers()
}
