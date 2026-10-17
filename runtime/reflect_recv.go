package runtime

import (
	"fmt"
)

func reflectValueRecv(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_recv expects 1 argument, got %d", len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueRecvError(err.Error()), nil
	}
	current, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return reflectValueRecvError(err.Error()), nil
	}
	if !ok {
		return reflectValueRecvError("reflect: Recv of invalid Value"), nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isWaitableTypeName(current.Type) {
		return reflectValueRecvError("reflect: Recv of non-chan Value"), nil
	}
	value, received, closed, err := waitableTryRecvValue(module, current, ctx.batch)
	if err != nil {
		return reflectValueRecvError(err.Error()), nil
	}
	request := &reflectRecvRequest{ctx: intrinsicContext{vm: ctx.vm, module: module, task: ctx.task, batch: ctx.batch}, waitable: current}
	if !received && !closed {
		return nil, request
	}
	return request.complete(value, received)
}

func (request *reflectRecvRequest) complete(value vmValue, received bool) ([]vmValue, error) {
	snapshot, err := reflectValueSnapshot(request.ctx, value)
	if err != nil {
		return reflectValueRecvError(err.Error()), nil
	}
	return []vmValue{
		snapshot,
		newVMValue("Bool", received),
		newVMValue("String", ""),
		newVMValue("Bool", true),
	}, nil
}

func reflectValueRecvError(message string) []vmValue {
	return []vmValue{
		zeroReflectValueValue(),
		newVMValue("Bool", false),
		newVMValue("String", message),
		newVMValue("Bool", false),
	}
}
