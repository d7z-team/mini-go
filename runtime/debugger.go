package runtime

import (
	"fmt"
	"sync"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type breakpointSource struct {
	ModulePath string
	File       string
}

type breakpointSet struct {
	requested  []int
	generation uint64
	active     map[int]struct{}
}

type debugEventKind string

const (
	debugEventBreakpoint debugEventKind = "breakpoint"
	debugEventStep       debugEventKind = "step"
	debugEventPause      debugEventKind = "pause"
	debugEventPanic      debugEventKind = "panic"
)

type debugLocal struct {
	ID    string
	Name  string
	Type  string
	Value vmValue
}

type debugFrame struct {
	ScopeID            int64
	SymbolsHash        string
	SourceHash         string
	Generation         uint64
	ProgramHash        string
	hasSymbols         bool
	ExecutionContextID int64
	ModulePath         string
	FunctionID         string
	PC                 int
	Loc                ir.Location
	Locals             []debugLocal
	Upvalues           []debugLocal
	Globals            []debugLocal
}

type debugEvent struct {
	Kind               debugEventKind
	RunID              int64
	Generation         uint64
	ProgramHash        string
	ExecutionContextID int64
	Frame              debugFrame
	Stack              []debugFrame
	Panic              vmValue
}

type debugPauseError struct {
	Event debugEvent
}

func (e debugPauseError) Error() string {
	loc := e.Event.Frame.Loc
	return fmt.Sprintf("debug pause: %s %s:%d:%d", e.Event.Kind, loc.File, loc.Line, loc.Column)
}

type Debugger struct {
	mu          sync.RWMutex
	bound       bool
	closed      bool
	breakpoints map[breakpointSource]breakpointSet
	events      []Event
	paused      *debugEvent
}

const maxDebugEvents = 256

type debugPauseState struct {
	frame      *frame
	functionID string
}

type debugResumePoint struct {
	TaskID     int64
	Generation uint64
	ModulePath string
	FunctionID string
	PC         int
	Active     bool
}

type debugStepState struct {
	TaskID     int64
	Mode       debugStepMode
	StartDepth int
	Active     bool
}

type debugStepMode string

const (
	debugStepInto debugStepMode = "into"
	debugStepOver debugStepMode = "over"
	debugStepOut  debugStepMode = "out"
)

func NewDebugger() *Debugger {
	return &Debugger{breakpoints: make(map[breakpointSource]breakpointSet)}
}

func (d *Debugger) replaceBreakpointsLocked(modulePath, file string, requested []int, generation uint64, active map[int]struct{}) {
	if d.breakpoints == nil {
		d.breakpoints = make(map[breakpointSource]breakpointSet)
	}
	source := breakpointSource{ModulePath: modulePath, File: file}
	if len(requested) == 0 {
		delete(d.breakpoints, source)
	} else {
		d.breakpoints[source] = breakpointSet{requested: append([]int(nil), requested...), generation: generation, active: active}
	}
}

func (d *Debugger) debugEvents() []Event {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.events) == 0 {
		return nil
	}
	return append([]Event(nil), d.events...)
}

func (d *Debugger) appendEvent(event debugEvent) {
	if d != nil {
		projected := event.publicEvent()
		d.mu.Lock()
		defer d.mu.Unlock()
		if event.Kind != debugEventPanic {
			paused := event
			d.paused = &paused
		}
		if len(d.events) == maxDebugEvents {
			copy(d.events, d.events[1:])
			d.events[len(d.events)-1] = projected
			return
		}
		d.events = append(d.events, projected)
	}
}

func (d *Debugger) pausedEvent() debugEvent {
	if d == nil {
		return debugEvent{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.paused == nil {
		return debugEvent{}
	}
	return *d.paused
}

func (d *Debugger) clearPausedEvent() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.paused = nil
	d.mu.Unlock()
}

// closeLocked releases guest references while the caller excludes revision readers.
func (d *Debugger) closeLocked() {
	d.closed = true
	d.breakpoints = nil
	d.events = nil
	d.paused = nil
}

func (d *Debugger) hasBreakpoint(generation uint64, modulePath, file string, line int) bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.breakpoints == nil {
		return false
	}
	set, ok := d.breakpoints[breakpointSource{ModulePath: modulePath, File: file}]
	if !ok {
		return false
	}
	if set.generation != generation {
		return false
	}
	_, ok = set.active[line]
	return ok
}

func (d *Debugger) hasActiveBreakpoints() bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.hasActiveBreakpointsLocked()
}

func (d *Debugger) hasActiveBreakpointsLocked() bool {
	for _, set := range d.breakpoints {
		if len(set.active) != 0 {
			return true
		}
	}
	return false
}
