package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const maxDebugReferences = 100_000

type ResolvedBreakpoint struct {
	RequestedLine int
	Line          int
	Column        int
	Verified      bool
}

// SetBreakpoints resolves source lines against the current revision and then
// atomically replaces the debugger breakpoints for that source.
func (i *Instance) SetBreakpoints(modulePath, file string, lines []int) ([]ResolvedBreakpoint, error) {
	if i == nil || i.vm == nil || !i.isOpen() {
		return nil, errors.New("instance is closed")
	}
	if strings.TrimSpace(modulePath) == "" {
		return nil, errors.New("breakpoint module path is required")
	}
	if strings.TrimSpace(file) == "" {
		return nil, errors.New("breakpoint file is required")
	}
	for _, line := range lines {
		if line <= 0 {
			return nil, errors.New("breakpoint line must be positive")
		}
	}
	debugger := i.vm.debugger
	debugger.mu.Lock()
	defer debugger.mu.Unlock()
	if !i.isOpen() || debugger.closed {
		return nil, errors.New("instance is closed")
	}
	revision := i.vm.revision.Load()
	if revision == nil {
		return nil, errors.New("instance has no active revision")
	}
	resolved, active, err := resolveBreakpointLines(revision, modulePath, file, lines)
	if err != nil {
		return nil, err
	}
	i.vm.debugger.replaceBreakpointsLocked(modulePath, file, lines, revision.generation, active)
	i.vm.breakpointsActive.Store(i.vm.debugger.hasActiveBreakpointsLocked())
	return resolved, nil
}

func resolveBreakpointLines(revision *instanceRevision, modulePath, file string, lines []int) ([]ResolvedBreakpoint, map[int]struct{}, error) {
	module, ok := revision.modules.module(modulePath)
	if !ok || module == nil || module.executable == nil {
		return nil, nil, fmt.Errorf("module %q is not loaded", modulePath)
	}
	if revision.symbols == nil {
		resolved := make([]ResolvedBreakpoint, len(lines))
		for index, line := range lines {
			resolved[index] = ResolvedBreakpoint{RequestedLine: line, Line: line}
		}
		return resolved, nil, nil
	}
	available := make(map[int]int)
	packageSymbols := revision.symbols.packages[modulePath]
	for _, function := range packageSymbols.functions {
		for _, instruction := range function.Locations {
			for _, location := range instruction.Points {
				if location.File != file || location.Line <= 0 {
					continue
				}
				column, exists := available[location.Line]
				if !exists || location.Column < column {
					available[location.Line] = location.Column
				}
			}
		}
	}
	ordered := make([]int, 0, len(available))
	for line := range available {
		ordered = append(ordered, line)
	}
	sort.Ints(ordered)
	resolved := make([]ResolvedBreakpoint, len(lines))
	active := make(map[int]struct{}, len(lines))
	for index, requested := range lines {
		resolved[index] = ResolvedBreakpoint{RequestedLine: requested, Line: requested}
		position := sort.SearchInts(ordered, requested)
		if requested > 0 && position < len(ordered) {
			line := ordered[position]
			resolved[index].Line, resolved[index].Column, resolved[index].Verified = line, available[line], true
			active[line] = struct{}{}
		}
	}
	return resolved, active, nil
}

func (vm *vm) refreshBreakpointsLocked(revision *instanceRevision) {
	if vm == nil || vm.debugger == nil || revision == nil {
		return
	}
	for source, set := range vm.debugger.breakpoints {
		_, resolved, err := resolveBreakpointLines(revision, source.ModulePath, source.File, set.requested)
		if err != nil {
			set.active = nil
			set.generation = revision.generation
			vm.debugger.breakpoints[source] = set
			continue
		}
		set.generation = revision.generation
		set.active = resolved
		vm.debugger.breakpoints[source] = set
	}
	vm.breakpointsActive.Store(vm.debugger.hasActiveBreakpointsLocked())
}

type DebugThread struct {
	ScopeID int64
	ID      int64
	Name    string
}

type DebugFrame struct {
	ScopeID          int64
	SymbolsHash      string
	SourceHash       string
	ID               int
	ThreadID         int64
	Generation       uint64
	ProgramHash      string
	HasSymbols       bool
	ModulePath       string
	FunctionID       string
	PC               int
	File             string
	Line             int
	Column           int
	PresentationHint string
}

type DebugScope struct {
	Name               string
	VariablesReference int
	NamedVariables     int
}

type DebugVariable struct {
	Name               string
	Type               string
	Value              string
	VariablesReference int
	IndexedVariables   int
	NamedVariables     int
}

type DebugSnapshot struct {
	Epoch       uint64
	Generation  uint64
	ProgramHash string
	Reason      string
	Threads     []DebugThread
	Frames      []DebugFrame
}

type debugReference struct {
	bindings []debugLocal
	value    *vmValue
}

type debugInspection struct {
	epoch      uint64
	hasSymbols bool
	snapshot   DebugSnapshot
	frames     map[int]*debugFrame
	references map[int]debugReference
	objects    map[debugObjectIdentity]int
}

type debugObjectIdentity struct {
	kind  string
	value any
}

func (e *Execution) buildDebugInspectionLocked() {
	if e == nil || e.pause == nil {
		e.debugInspection = nil
		return
	}
	e.debugEpoch++
	inspection := &debugInspection{epoch: e.debugEpoch, hasSymbols: e.pause.Frame.hasSymbols, frames: make(map[int]*debugFrame), references: make(map[int]debugReference), objects: make(map[debugObjectIdentity]int)}
	inspection.snapshot = DebugSnapshot{Epoch: e.debugEpoch, Generation: e.pause.Generation, ProgramHash: e.pause.ProgramHash, Reason: string(e.pause.Kind)}
	threads := make(map[int64]struct{})
	for index := range e.pause.Stack {
		frame := &e.pause.Stack[index]
		if _, exists := threads[frame.ExecutionContextID]; !exists {
			threads[frame.ExecutionContextID] = struct{}{}
			inspection.snapshot.Threads = append(inspection.snapshot.Threads, DebugThread{ID: frame.ExecutionContextID, ScopeID: frame.ScopeID, Name: fmt.Sprintf("task %d / scope %d", frame.ExecutionContextID, frame.ScopeID)})
		}
		e.nextDebugReference++
		frameID := e.nextDebugReference
		inspection.frames[frameID] = frame
		inspection.snapshot.Frames = append(inspection.snapshot.Frames, DebugFrame{
			ScopeID: frame.ScopeID, SymbolsHash: frame.SymbolsHash, SourceHash: frame.SourceHash,
			ID: frameID, ThreadID: frame.ExecutionContextID, Generation: frame.Generation, ProgramHash: frame.ProgramHash,
			HasSymbols: frame.hasSymbols, ModulePath: frame.ModulePath, FunctionID: frame.FunctionID, PC: frame.PC,
			File: frame.Loc.File, Line: frame.Loc.Line, Column: frame.Loc.Column,
		})
	}
	e.debugInspection = inspection
}

func (e *Execution) DebugSnapshot() (DebugSnapshot, error) {
	if e == nil {
		return DebugSnapshot{}, errors.New("execution is not paused")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if (e.state != ExecutionPaused && !e.instance.isBackgroundPaused(e)) || e.debugInspection == nil {
		return DebugSnapshot{}, errors.New("execution is not paused")
	}
	result := e.debugInspection.snapshot
	result.Threads = append([]DebugThread(nil), result.Threads...)
	result.Frames = append([]DebugFrame(nil), result.Frames...)
	return result, nil
}

func (e *Execution) DebugScopes(frameID int) ([]DebugScope, error) {
	if e == nil {
		return nil, errors.New("execution is not paused")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if (e.state != ExecutionPaused && !e.instance.isBackgroundPaused(e)) || e.debugInspection == nil {
		return nil, errors.New("execution is not paused")
	}
	frame, ok := e.debugInspection.frames[frameID]
	if !ok {
		return nil, errors.New("stale debug frame reference")
	}
	if !frame.hasSymbols {
		return nil, ErrDebugSymbolsUnavailable
	}
	groups := []struct {
		name   string
		values []debugLocal
	}{{"Locals", frame.Locals}, {"Upvalues", frame.Upvalues}, {"Globals", frame.Globals}}
	result := make([]DebugScope, 0, len(groups))
	for _, group := range groups {
		if len(group.values) == 0 {
			continue
		}
		reference, err := e.addDebugReference(debugReference{bindings: group.values})
		if err != nil {
			return nil, err
		}
		result = append(result, DebugScope{Name: group.name, VariablesReference: reference, NamedVariables: len(group.values)})
	}
	return result, nil
}

func (e *Execution) DebugVariables(reference, start, count int) ([]DebugVariable, error) {
	if e == nil {
		return nil, errors.New("execution is not paused")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if (e.state != ExecutionPaused && !e.instance.isBackgroundPaused(e)) || e.debugInspection == nil {
		return nil, errors.New("execution is not paused")
	}
	entry, ok := e.debugInspection.references[reference]
	if !ok {
		if !e.debugInspection.hasSymbols {
			return nil, ErrDebugSymbolsUnavailable
		}
		return nil, errors.New("stale debug variable reference")
	}
	if start < 0 || count < 0 {
		return nil, errors.New("debug variable range must be non-negative")
	}
	var values []debugLocal
	if entry.bindings != nil {
		values = entry.bindings
	} else if entry.value != nil {
		values = debugValueChildren(*entry.value)
	}
	if start > len(values) {
		start = len(values)
	}
	end := len(values)
	if count > 0 && count < end-start {
		end = start + count
	}
	result := make([]DebugVariable, 0, end-start)
	for _, binding := range values[start:end] {
		variable := DebugVariable{Name: binding.Name, Type: binding.Type, Value: debugValueText(binding.Value)}
		children := debugValueChildren(binding.Value)
		if len(children) != 0 {
			childReference := 0
			identity, reusable := debugValueIdentity(binding.Value)
			if reusable {
				childReference = e.debugInspection.objects[identity]
			}
			if childReference == 0 {
				value := binding.Value
				var err error
				childReference, err = e.addDebugReference(debugReference{value: &value})
				if err != nil {
					return nil, err
				}
				if reusable {
					e.debugInspection.objects[identity] = childReference
				}
			}
			variable.VariablesReference = childReference
			if _, indexed := binding.Value.Data.(*vmArray); indexed {
				variable.IndexedVariables = len(children)
			} else if _, indexed := binding.Value.Data.(*vmSlice); indexed {
				variable.IndexedVariables = len(children)
			} else {
				variable.NamedVariables = len(children)
			}
		}
		result = append(result, variable)
	}
	return result, nil
}

func (e *Execution) addDebugReference(reference debugReference) (int, error) {
	if len(e.debugInspection.frames)+len(e.debugInspection.references) >= maxDebugReferences {
		return 0, errors.New("debug reference limit reached")
	}
	e.nextDebugReference++
	e.debugInspection.references[e.nextDebugReference] = reference
	return e.nextDebugReference, nil
}

func debugValueIdentity(value vmValue) (debugObjectIdentity, bool) {
	switch data := value.Data.(type) {
	case *vmSlice:
		return debugObjectIdentity{kind: "slice", value: data}, data != nil
	case *vmMap:
		return debugObjectIdentity{kind: "map", value: data}, data != nil
	default:
		return debugObjectIdentity{}, false
	}
}

func debugValueChildren(value vmValue) []debugLocal {
	var result []debugLocal
	switch data := value.Data.(type) {
	case *vmSlice:
		items, _ := sliceValues(value)
		for index, item := range items {
			result = append(result, debugLocal{ID: strconv.Itoa(index), Name: fmt.Sprintf("[%d]", index), Type: item.Type.String(), Value: item})
		}
	case *vmArray:
		values := data.values()
		for index, item := range values {
			result = append(result, debugLocal{ID: strconv.Itoa(index), Name: fmt.Sprintf("[%d]", index), Type: item.Type.String(), Value: item})
		}
	case *vmStruct:
		if data == nil || data.schema == nil {
			return nil
		}
		for index, field := range data.schema.fields {
			value := zeroVMValue(field.RuntimeType.String())
			if stored, ok := data.fieldAt(index); ok {
				value = stored
			}
			result = append(result, debugLocal{ID: field.Name, Name: field.Name, Type: value.Type.String(), Value: value})
		}
	case *vmMap:
		if data == nil {
			return nil
		}
		snapshot := data.snapshot()
		for _, key := range sortedVMMapKeys(snapshot) {
			entry := snapshot[key]
			result = append(result, debugLocal{ID: key.Text, Name: "[" + debugValueText(entry.Key) + "]", Type: entry.Value.Type.String(), Value: entry.Value})
		}
	}
	return result
}
