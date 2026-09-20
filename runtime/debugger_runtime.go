package runtime

import ir "github.com/d7z-team/mini-go/runtime/bytecode"

func (vm *vm) appendDebugEvent(event debugEvent) {
	if vm == nil || vm.debugger == nil {
		return
	}
	vm.debugger.appendEvent(event)
}

func (vm *vm) checkDebugPause(task *executionTask, frame *frame, functionID string, pc int) (debugPauseError, bool) {
	if vm == nil || frame == nil || frame.module == nil || frame.module.executable == nil {
		return debugPauseError{}, false
	}
	if vm.hostPauseRequested.Load() && vm.hostPauseRequested.Swap(false) {
		loc, ok := frame.revision.symbols.nearestLocation(frame.module.modulePath(), functionID, pc)
		if !ok {
			loc = ir.Location{}
		}
		event := vm.debugPauseEvent(task, debugEventPause, frame, functionID, pc, loc)
		vm.appendDebugEvent(event)
		return debugPauseError{Event: event}, true
	}
	if vm.debugger == nil {
		return debugPauseError{}, false
	}
	locations := frame.revision.symbols.locations(frame.module.modulePath(), functionID, pc)
	if len(locations) == 0 {
		if frame.revision.symbols == nil && vm.shouldPauseForStep(task) {
			vm.debugStep = debugStepState{}
			vm.debugStepActive.Store(false)
			event := vm.debugPauseEvent(task, debugEventStep, frame, functionID, pc, ir.Location{})
			vm.appendDebugEvent(event)
			return debugPauseError{Event: event}, true
		}
		return debugPauseError{}, false
	}
	loc := locations[0]
	modulePath := frame.module.executable.Artifact.Module.Path
	if vm.skipDebugPause.Active &&
		vm.skipDebugPause.TaskID == task.id &&
		vm.skipDebugPause.Generation == frame.revisionGeneration() &&
		vm.skipDebugPause.ModulePath == modulePath &&
		vm.skipDebugPause.FunctionID == functionID &&
		vm.skipDebugPause.PC == pc {
		vm.skipDebugPause = debugResumePoint{}
		return debugPauseError{}, false
	}
	if vm.shouldPauseForStep(task) {
		vm.debugStep = debugStepState{}
		vm.debugStepActive.Store(false)
		event := vm.debugPauseEvent(task, debugEventStep, frame, functionID, pc, loc)
		vm.appendDebugEvent(event)
		return debugPauseError{Event: event}, true
	}
	if !vm.breakpointsActive.Load() {
		return debugPauseError{}, false
	}
	matched := false
	for _, candidate := range locations {
		if vm.debugger.hasBreakpoint(frame.revisionGeneration(), modulePath, candidate.File, candidate.Line) {
			loc = candidate
			matched = true
			break
		}
	}
	if !matched {
		return debugPauseError{}, false
	}
	event := vm.debugPauseEvent(task, debugEventBreakpoint, frame, functionID, pc, loc)
	vm.appendDebugEvent(event)
	return debugPauseError{Event: event}, true
}

func (vm *vm) debugPauseEvent(task *executionTask, kind debugEventKind, frame *frame, functionID string, pc int, loc ir.Location) debugEvent {
	stack := task.debugStack(frame, functionID, pc, loc)
	event := debugEvent{
		Kind:               kind,
		RunID:              task.scope.id,
		Generation:         frame.revisionGeneration(),
		ProgramHash:        frame.revisionHash(),
		ExecutionContextID: frame.executionContextID,
		Stack:              stack,
	}
	if len(stack) != 0 {
		event.Frame = stack[0]
	}
	return event
}

func (vm *vm) debugPanicEvent(task *executionTask, frame *frame, functionID string, pc int, value vmValue) (debugEvent, bool) {
	if vm == nil || vm.debugger == nil || frame == nil || frame.module == nil || frame.module.executable == nil {
		return debugEvent{}, false
	}
	loc, _ := frame.revision.symbols.nearestLocation(frame.module.modulePath(), functionID, pc)
	stack := task.debugStack(frame, functionID, pc, loc)
	event := debugEvent{
		Kind:               debugEventPanic,
		RunID:              task.scope.id,
		Generation:         frame.revisionGeneration(),
		ProgramHash:        frame.revisionHash(),
		ExecutionContextID: frame.executionContextID,
		Stack:              stack,
		Panic:              value,
	}
	if len(stack) != 0 {
		event.Frame = stack[0]
	}
	return event, true
}

func (task *executionTask) debugStack(current *frame, functionID string, pc int, loc ir.Location) []debugFrame {
	var scopeID int64
	if task != nil && task.scope != nil {
		scopeID = task.scope.id
	}
	if task == nil || len(task.frames) == 0 {
		return []debugFrame{current.debugFrame(functionID, pc, loc, scopeID)}
	}
	out := make([]debugFrame, 0, len(task.debugParents)+len(task.frames))
	for i := len(task.frames) - 1; i >= 0; i-- {
		frame := task.frames[i].frame
		if frame == nil {
			continue
		}
		if frame != current && isHiddenGeneratedFrame(frame) {
			continue
		}
		framePC := frame.pc
		frameFunctionID := frame.function.Decl.ID
		var frameLoc ir.Location
		if frame == current {
			framePC = pc
			frameFunctionID = functionID
			frameLoc = loc
		} else {
			if framePC > 0 {
				framePC--
			}
			frameLoc, _ = frame.revision.symbols.nearestLocation(frame.module.modulePath(), frameFunctionID, framePC)
		}
		out = append(out, frame.debugFrame(frameFunctionID, framePC, frameLoc, scopeID))
	}
	for i := len(task.debugParents) - 1; i >= 0; i-- {
		out = append(out, task.debugParents[i])
	}
	if len(out) == 0 {
		return []debugFrame{current.debugFrame(functionID, pc, loc, scopeID)}
	}
	return out
}

func isHiddenGeneratedFrame(frame *frame) bool {
	if frame == nil || frame.revision == nil || frame.module == nil {
		return false
	}
	symbols, ok := frame.revision.symbols.function(frame.module.modulePath(), frame.function.Decl.ID)
	return ok && symbols.Generated
}

func (f *frame) debugFrame(functionID string, pc int, loc ir.Location, scopeID int64) debugFrame {
	if f == nil || f.module == nil || f.module.executable == nil {
		return debugFrame{ScopeID: scopeID, FunctionID: functionID, PC: pc, Loc: loc}
	}
	return debugFrame{
		ScopeID:            scopeID,
		SymbolsHash:        f.revision.symbolsHash(),
		SourceHash:         f.revision.symbols.sourceHash(f.module.modulePath(), loc.File),
		Generation:         f.revisionGeneration(),
		ProgramHash:        f.revisionHash(),
		hasSymbols:         f.revision != nil && f.revision.symbols != nil,
		ExecutionContextID: f.executionContextID,
		ModulePath:         f.module.executable.Artifact.Module.Path,
		FunctionID:         functionID,
		PC:                 pc,
		Loc:                loc,
		Locals:             f.debugLocals(pc),
		Upvalues:           f.debugUpvalues(),
		Globals:            f.debugGlobals(),
	}
}

func (f *frame) revisionGeneration() uint64 {
	if f == nil || f.revision == nil {
		return 0
	}
	return f.revision.generation
}

func (f *frame) revisionHash() string {
	if f == nil || f.revision == nil || f.revision.code == nil {
		return ""
	}
	return f.revision.code.image.Hash
}

func (f *frame) debugLocals(pc int) []debugLocal {
	function, ok := f.functionSymbols()
	if !ok || function.Generated {
		return nil
	}
	locals := make(map[string]ir.LocalSymbol, len(function.Locals))
	for _, local := range function.Locals {
		locals[local.ID] = local
	}
	out := make([]debugLocal, 0, len(f.function.Decl.Locals))
	for index, local := range f.function.Decl.Locals {
		symbol, exists := locals[local.ID]
		if !exists || symbol.Generated || !debugScopeContainsPC(function.Scopes, symbol.Scope, pc) {
			continue
		}
		if index >= len(f.localCells) || f.localCells[index] == nil {
			continue
		}
		slot := f.localCells[index]
		out = append(out, debugLocal{
			ID:    local.ID,
			Name:  symbol.Name,
			Type:  slot.typ.String(),
			Value: slot.load(),
		})
	}
	return out
}

func debugScopeContainsPC(scopes []ir.DebugScope, scopeID, pc int) bool {
	if scopeID == 0 {
		return true
	}
	for _, scope := range scopes {
		if scope.ID != scopeID {
			continue
		}
		for _, pcRange := range scope.Ranges {
			if pc >= pcRange.Start && pc < pcRange.End {
				return true
			}
		}
		return false
	}
	return false
}

func (f *frame) debugUpvalues() []debugLocal {
	function, ok := f.functionSymbols()
	if !ok {
		return nil
	}
	upvalues := make(map[string]string, len(function.Upvalues))
	for _, upvalue := range function.Upvalues {
		upvalues[upvalue.ID] = upvalue.Name
	}
	out := make([]debugLocal, 0, len(f.function.Decl.Upvalues))
	for index, upvalue := range f.function.Decl.Upvalues {
		if index >= len(f.upvalueCells) || f.upvalueCells[index] == nil {
			continue
		}
		slot := f.upvalueCells[index]
		out = append(out, debugLocal{
			ID:    upvalue.ID,
			Name:  upvalues[upvalue.ID],
			Type:  slot.typ.String(),
			Value: slot.load(),
		})
	}
	return out
}

func (f *frame) debugGlobals() []debugLocal {
	if f == nil || f.revision == nil || f.revision.symbols == nil || f.module == nil || f.module.executable == nil {
		return nil
	}
	out := make([]debugLocal, 0, len(f.module.executable.Artifact.Globals))
	for _, global := range f.module.executable.Artifact.Globals {
		slot, ok := f.module.state.globals[global.ID]
		if !ok || slot == nil {
			continue
		}
		out = append(out, debugLocal{
			ID:    global.ID,
			Name:  f.revision.symbols.globalName(f.module.modulePath(), global.ID),
			Type:  slot.typ.String(),
			Value: slot.load(),
		})
	}
	return out
}

func (f *frame) functionSymbols() (ir.FunctionSymbols, bool) {
	if f == nil || f.revision == nil || f.module == nil {
		return ir.FunctionSymbols{}, false
	}
	return f.revision.symbols.function(f.module.modulePath(), f.function.Decl.ID)
}
