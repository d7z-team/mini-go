package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func ProjectExecutionError(err error) (ExecutionError, error) {
	if err == nil {
		return ExecutionError{}, errors.New("nil execution error")
	}
	var resourceLimit ResourceLimitError
	if errors.As(err, &resourceLimit) {
		out := NewExecutionError(ErrorRuntime, resourceLimit.Code, resourceLimit.Error())
		return out, out.Validate()
	}
	var runtimeErr Error
	if errors.As(err, &runtimeErr) {
		var stepLimit StepLimitError
		if errors.As(runtimeErr.Err, &stepLimit) {
			out := NewExecutionError(ErrorStepLimit, "execution.step_limit", stepLimit.Error())
			return out, out.Validate()
		}
		if errors.As(runtimeErr.Err, &resourceLimit) {
			out := NewExecutionError(ErrorRuntime, resourceLimit.Code, resourceLimit.Error())
			return out, out.Validate()
		}
		out := NewExecutionError(ErrorRuntime, "runtime.error", runtimeErr.Error())
		out.Generation = runtimeErr.Generation
		out.ProgramHash = runtimeErr.ProgramHash
		out.ModulePath = runtimeErr.ModulePath
		out.FunctionID = runtimeErr.FunctionID
		out.PC = runtimeErr.PC
		out.Op = runtimeErr.Op
		out.RouteID = runtimeErr.RouteID
		out.ExecutionContextID = runtimeErr.ExecutionContextID
		if runtimeErr.Loc != nil {
			loc := projectLocation(*runtimeErr.Loc)
			out.Loc = &loc
		}
		if runtimeErr.Err != nil {
			var blocked AllBlockedError
			var panicErr panicError
			if errors.As(runtimeErr.Err, &blocked) {
				out.Kind = ErrorBlocked
				out.Code = "execution.blocked"
				out.Reason = executionBlockedReason(blocked)
				out.Stack = projectBlockedStack(blocked)
			} else if errors.As(runtimeErr.Err, &panicErr) {
				out.Kind = ErrorPanic
				out.Code = "panic"
				out.Message = "panic"
				out.Generation = panicErr.Generation
				out.ProgramHash = panicErr.ProgramHash
				out.ModulePath = firstNonEmpty(panicErr.ModulePath, runtimeErr.ModulePath)
				out.FunctionID = firstNonEmpty(panicErr.FunctionID, runtimeErr.FunctionID)
				out.PC = panicErr.PC
				if panicErr.Loc != nil {
					loc := projectLocation(*panicErr.Loc)
					out.Loc = &loc
				}
				panicValue := projectDebugValue(panicErr.Value)
				out.Panic = &panicValue
			}
		}
		if len(runtimeErr.stack) != 0 {
			out.Stack = projectRuntimeStack(runtimeErr.stack)
		} else if len(out.Stack) == 0 {
			out.Stack = projectExecutionStack(out)
		}
		return out, out.Validate()
	}
	var panicErr panicError
	if errors.As(err, &panicErr) {
		out := NewExecutionError(ErrorPanic, "panic", "panic")
		out.Generation = panicErr.Generation
		out.ProgramHash = panicErr.ProgramHash
		out.ModulePath = panicErr.ModulePath
		out.FunctionID = panicErr.FunctionID
		out.PC = panicErr.PC
		if panicErr.Loc != nil {
			loc := projectLocation(*panicErr.Loc)
			out.Loc = &loc
		}
		panicValue := projectDebugValue(panicErr.Value)
		out.Panic = &panicValue
		out.Stack = projectExecutionStack(out)
		return out, out.Validate()
	}
	var blocked AllBlockedError
	if errors.As(err, &blocked) {
		out := NewExecutionError(ErrorBlocked, "execution.blocked", blocked.Error())
		out.Reason = executionBlockedReason(blocked)
		out.Stack = projectBlockedStack(blocked)
		if len(blocked.Contexts) != 0 {
			first := blocked.Contexts[0]
			out.Generation, out.ProgramHash = first.Revision.Generation, first.Revision.Hash
			out.ModulePath, out.FunctionID = first.ModulePath, first.FunctionID
			out.PC, out.Op, out.ExecutionContextID = first.PC, first.Op, first.ExecutionContextID
			if first.Loc != nil {
				loc := projectLocation(*first.Loc)
				out.Loc = &loc
			}
		}
		return out, out.Validate()
	}
	var stepLimit StepLimitError
	if errors.As(err, &stepLimit) {
		out := NewExecutionError(ErrorStepLimit, "execution.step_limit", stepLimit.Error())
		out.Reason = fmt.Sprintf("max_steps=%d", stepLimit.MaxSteps)
		return out, out.Validate()
	}
	var validationErr ir.ValidationError
	if errors.As(err, &validationErr) {
		issue, _ := ir.ValidationIssueFromError(err)
		out := NewExecutionError(ErrorInternal, issue.Code, err.Error())
		return out, out.Validate()
	}
	out := NewExecutionError(ErrorInternal, "execution.internal", err.Error())
	return out, out.Validate()
}

func projectRuntimeStack(stack []runtimeStackFrame) []Frame {
	out := make([]Frame, 0, len(stack))
	for _, frame := range stack {
		projected := Frame{
			Generation:         frame.Revision.Generation,
			ProgramHash:        frame.Revision.Hash,
			ExecutionContextID: frame.ExecutionContextID,
			ModulePath:         frame.ModulePath,
			FunctionID:         frame.FunctionID,
			PC:                 frame.PC,
		}
		if frame.Loc != nil {
			projected.Loc = projectLocation(*frame.Loc)
		}
		out = append(out, projected)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func executionBlockedReason(blocked AllBlockedError) string {
	if len(blocked.Contexts) != 0 && strings.TrimSpace(blocked.Contexts[0].Reason) != "" {
		return blocked.Contexts[0].Reason
	}
	return blocked.Error()
}

func projectBlockedStack(blocked AllBlockedError) []Frame {
	out := make([]Frame, 0, len(blocked.Contexts))
	for _, context := range blocked.Contexts {
		frame := Frame{
			Generation: context.Revision.Generation, ProgramHash: context.Revision.Hash,
			ExecutionContextID: context.ExecutionContextID, ModulePath: context.ModulePath,
			FunctionID: context.FunctionID, PC: context.PC,
		}
		if context.Loc != nil {
			frame.Loc = projectLocation(*context.Loc)
		}
		out = append(out, frame)
	}
	return out
}

func projectExecutionStack(err ExecutionError) []Frame {
	if strings.TrimSpace(err.FunctionID) == "" {
		return nil
	}
	frame := Frame{
		Generation:         err.Generation,
		ProgramHash:        err.ProgramHash,
		ExecutionContextID: err.ExecutionContextID,
		ModulePath:         err.ModulePath,
		FunctionID:         err.FunctionID,
		PC:                 err.PC,
	}
	if err.Loc != nil {
		frame.Loc = *err.Loc
	}
	return []Frame{frame}
}

func (e debugEvent) publicEvent() Event {
	out := NewEvent(projectEventKind(e.Kind))
	out.RunID = e.RunID
	out.Generation = e.Generation
	out.ProgramHash = e.ProgramHash
	out.ExecutionContextID = e.ExecutionContextID
	out.Frame = projectDebugFrame(e.Frame)
	out.Stack = make([]Frame, 0, len(e.Stack))
	for _, frame := range e.Stack {
		out.Stack = append(out.Stack, projectDebugFrame(frame))
	}
	if e.Kind == debugEventPanic {
		value := projectDebugValue(e.Panic)
		out.Panic = &value
	}
	return out
}

func (d *Debugger) Events() []Event {
	return d.debugEvents()
}

func projectEventKind(kind debugEventKind) EventKind {
	switch kind {
	case debugEventBreakpoint:
		return EventBreakpoint
	case debugEventStep:
		return EventStep
	case debugEventPause:
		return EventPause
	case debugEventPanic:
		return EventPanic
	default:
		return EventKind(kind)
	}
}

func projectDebugFrame(frame debugFrame) Frame {
	return Frame{
		Generation:         frame.Generation,
		ProgramHash:        frame.ProgramHash,
		ExecutionContextID: frame.ExecutionContextID,
		ModulePath:         frame.ModulePath,
		FunctionID:         frame.FunctionID,
		PC:                 frame.PC,
		Loc:                projectLocation(frame.Loc),
		Locals:             projectDebugBindings(frame.Locals),
		Upvalues:           projectDebugBindings(frame.Upvalues),
		Globals:            projectDebugBindings(frame.Globals),
	}
}

func projectLocation(loc ir.Location) Location {
	return Location{
		File:   loc.File,
		Line:   loc.Line,
		Column: loc.Column,
	}
}

func projectDebugBindings(values []debugLocal) []Binding {
	if len(values) == 0 {
		return nil
	}
	out := make([]Binding, 0, len(values))
	for _, value := range values {
		out = append(out, Binding{
			ID:    value.ID,
			Name:  value.Name,
			Type:  value.Type,
			Value: projectDebugValue(value.Value),
		})
	}
	return out
}

func projectDebugValue(value vmValue) Value {
	out := Value{
		Type: value.Type.String(),
		Text: debugValueText(value),
	}
	switch data := value.materializedData().(type) {
	case nil:
		out.Kind = ValueNil
		out.Nil = true
	case bool:
		out.Kind = ValueBool
		v := data
		out.Bool = &v
	case int64:
		out.Kind = ValueInt64
		v := data
		out.Int64 = &v
	case uint64:
		out.Kind = ValueUint64
		v := data
		out.Uint64 = &v
	case float64:
		out.Kind = ValueFloat64
		v := data
		out.Float64 = &v
	case complex128:
		out.Kind = ValueComplex128
		out.Complex128 = &Complex128Value{Real: real(data), Imag: imag(data)}
	case string:
		out.Kind = ValueString
		v := data
		out.String = &v
	case *vmSlice:
		out.Kind = ValueArray
		items, _ := sliceValues(value)
		out.Items = make([]Value, 0, len(items))
		for _, item := range items {
			out.Items = append(out.Items, projectDebugValue(item))
		}
	case *vmArray:
		values := data.values()
		out.Kind = ValueArray
		out.Items = make([]Value, 0, len(values))
		for _, item := range values {
			out.Items = append(out.Items, projectDebugValue(item))
		}
	case *vmStruct:
		out.Kind = ValueStruct
		if data == nil || data.schema == nil {
			break
		}
		out.Fields = make([]Binding, 0, len(data.schema.fields))
		for index, field := range data.schema.fields {
			value := zeroVMValue(field.RuntimeType.String())
			if stored, ok := data.fieldAt(index); ok {
				value = stored
			}
			out.Fields = append(out.Fields, Binding{
				ID:    field.Name,
				Name:  field.Name,
				Type:  value.Type.String(),
				Value: projectDebugValue(value),
			})
		}
	case *vmMap:
		out.Kind = ValueMap
		snapshot := data.snapshot()
		keys := sortedVMMapKeys(snapshot)
		out.Entries = make([]MapEntry, 0, len(keys))
		for _, key := range keys {
			entry := snapshot[key]
			out.Entries = append(out.Entries, MapEntry{
				Key:   projectDebugValue(entry.Key),
				Value: projectDebugValue(entry.Value),
			})
		}
	default:
		out.Kind = ValueOpaque
	}
	return out
}

func debugValueText(value vmValue) string {
	switch data := value.materializedData().(type) {
	case nil:
		return "nil"
	case bool:
		if data {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(data, 10)
	case uint64:
		return strconv.FormatUint(data, 10)
	case float64:
		return fmt.Sprintf("%g", data)
	case complex128:
		return fmt.Sprintf("(%g+%gi)", real(data), imag(data))
	case string:
		return data
	case *vmSlice:
		if data == nil {
			return fmt.Sprintf("%s(len=0)", value.Type)
		}
		return fmt.Sprintf("%s(len=%d)", value.Type, data.Len)
	case *vmArray:
		return fmt.Sprintf("%s(len=%d)", value.Type, data.Len)
	case *vmStruct:
		if data == nil || data.schema == nil {
			return fmt.Sprintf("%s(fields=0)", value.Type)
		}
		return fmt.Sprintf("%s(fields=%d)", value.Type, len(data.schema.fields))
	case *vmMap:
		if data == nil {
			return fmt.Sprintf("%s(len=0)", value.Type)
		}
		return fmt.Sprintf("%s(len=%d)", value.Type, data.length())
	default:
		if !value.Type.Valid() {
			return fmt.Sprintf("%T", data)
		}
		return value.Type.String()
	}
}
