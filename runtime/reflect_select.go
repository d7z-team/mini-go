package runtime

import (
	"errors"
	"fmt"
)

const (
	reflectSelectSend    = 1
	reflectSelectRecv    = 2
	reflectSelectDefault = 3
)

type reflectSelectCaseState struct {
	index   int
	dir     int
	channel vmValue
	send    vmValue
}

func reflectSelect(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.select expects 1 argument, got %d", len(args))
	}
	values, ok := sliceValues(args[0])
	if !ok {
		return reflectSelectError("reflect.Select: cases must be []SelectCase"), nil
	}
	module := reflectRelationModule(ctx)
	cases := make([]reflectSelectCaseState, 0, len(values))
	defaultIndex := -1
	for index, value := range values {
		fields, ok := materializeStructValue(value.Data)
		if !ok {
			return reflectSelectError(fmt.Sprintf("reflect.Select: invalid case %d", index)), nil
		}
		direction, err := asInt64(fields["Dir"])
		if err != nil || direction < reflectSelectSend || direction > reflectSelectDefault {
			return reflectSelectError(fmt.Sprintf("reflect.Select: invalid Dir for case %d", index)), nil
		}
		if direction == reflectSelectDefault {
			if defaultIndex >= 0 {
				return reflectSelectError("reflect.Select: multiple default cases"), nil
			}
			defaultIndex = index
			continue
		}
		channel, present, message := reflectCurrentValueArg(fields["Chan"], "Select")
		if !present {
			payload, payloadErr := reflectValuePayload(fields["Chan"])
			if payloadErr == nil && !reflectBoolField(payload.get("valid")) {
				continue
			}
			return reflectSelectError(message), nil
		}
		if module == nil || !module.isWaitableTypeName(channel.Type) {
			return reflectSelectError(fmt.Sprintf("reflect.Select: case %d uses non-chan value", index)), nil
		}
		state := reflectSelectCaseState{index: index, dir: int(direction), channel: channel}
		if direction == reflectSelectSend {
			send, present, message := reflectCurrentValueArg(fields["Send"], "Select")
			if !present {
				return reflectSelectError(message), nil
			}
			state.send = send
		}
		cases = append(cases, state)
	}

	operands := make([]channelSelectCase, len(cases))
	indexes := make([]int, len(cases))
	for index, selected := range cases {
		operands[index] = channelSelectCase{channel: selected.channel, send: selected.dir == reflectSelectSend, value: selected.send}
		indexes[index] = selected.index
	}
	selection, err := ctx.vm.prepareChannelSelection(ctx.task, module, operands)
	if err != nil {
		var limit ResourceLimitError
		if errors.As(err, &limit) {
			return nil, err
		}
		return reflectSelectError(err.Error()), nil
	}
	request := &reflectSelectRequest{ctx: ctx, selection: selection, indexes: indexes}
	if selection.tryCommit() {
		return request.complete()
	}
	if defaultIndex >= 0 {
		return reflectSelectResult(defaultIndex, zeroReflectValueValue(), false), nil
	}
	selection.register()
	return nil, request
}

func (request *reflectSelectRequest) complete() ([]vmValue, error) {
	selection := request.selection
	if selection.err != nil {
		return reflectSelectError(selection.err.Error()), nil
	}
	index := selection.index
	if index < 0 || index >= len(request.indexes) {
		return nil, fmt.Errorf("reflect.Select: invalid selected index %d", index)
	}
	if selection.cases[index].send {
		return reflectSelectResult(request.indexes[index], zeroReflectValueValue(), false), nil
	}
	snapshot, err := reflectValueSnapshot(request.ctx, selection.value)
	if err != nil {
		return reflectSelectError(err.Error()), nil
	}
	return reflectSelectResult(request.indexes[index], snapshot, selection.ok), nil
}

func reflectSelectResult(index int, value vmValue, received bool) []vmValue {
	return []vmValue{
		newVMValue("Int", int64(index)), value, newVMValue("Bool", received),
		newVMValue("String", ""), newVMValue("Bool", true),
	}
}

func reflectSelectError(message string) []vmValue {
	return []vmValue{
		newVMValue("Int", int64(0)), zeroReflectValueValue(), newVMValue("Bool", false),
		newVMValue("String", message), newVMValue("Bool", false),
	}
}
