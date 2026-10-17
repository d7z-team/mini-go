package runtime

import "fmt"

func reflectMakeChan(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.make_chan expects 2 arguments, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	info, err := reflectResolvedTypeInfo(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if info.Kind != "chan" && info.Kind != "recv_chan" && info.Kind != "send_chan" {
		return reflectValueError("reflect.MakeChan of non-chan type"), nil
	}
	if info.Kind != "chan" {
		return reflectValueError("reflect.MakeChan: unidirectional channel type"), nil
	}
	buffer, err := asInt64(args[1])
	if err != nil || buffer < 0 {
		return reflectValueError("reflect.MakeChan: negative buffer size"), nil
	}
	module := reflectRelationModule(ctx)
	value, err := makeWaitableValueWithBatch(module, typ, newVMValue("Int", buffer), ctx.batch)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	out, err := reflectValueSnapshot(ctx, value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectValueSend(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_send expects 2 arguments, got %d", len(args))
	}
	channel, ok, message := reflectCurrentValueArg(args[0], "Send")
	if !ok {
		return []vmValue{newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	value, ok, message := reflectCurrentValueArg(args[1], "Send")
	if !ok {
		return []vmValue{newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isWaitableTypeName(channel.Type) {
		return []vmValue{newVMValue("String", "reflect: Send of non-chan Value"), newVMValue("Bool", false)}, nil
	}
	return nil, &reflectSendRequest{ctx: intrinsicContext{vm: ctx.vm, module: module, task: ctx.task, batch: ctx.batch}, waitable: channel, value: value}
}

func reflectValueTryRecv(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_try_recv expects 1 argument, got %d", len(args))
	}
	channel, ok, message := reflectCurrentValueArg(args[0], "TryRecv")
	if !ok {
		return reflectValueRecvError(message), nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isWaitableTypeName(channel.Type) {
		return reflectValueRecvError("reflect: TryRecv of non-chan Value"), nil
	}
	value, received, closed, err := waitableTryRecvValue(module, channel, ctx.batch)
	if err != nil {
		return reflectValueRecvError(err.Error()), nil
	}
	if !received && !closed {
		return []vmValue{zeroReflectValueValue(), newVMValue("Bool", false), newVMValue("String", ""), newVMValue("Bool", true)}, nil
	}
	snapshot, err := reflectValueSnapshot(ctx, value)
	if err != nil {
		return reflectValueRecvError(err.Error()), nil
	}
	return []vmValue{snapshot, newVMValue("Bool", received), newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueTrySend(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_try_send expects 2 arguments, got %d", len(args))
	}
	channel, ok, message := reflectCurrentValueArg(args[0], "TrySend")
	if !ok {
		return reflectTrySendError(message), nil
	}
	value, ok, message := reflectCurrentValueArg(args[1], "TrySend")
	if !ok {
		return reflectTrySendError(message), nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isWaitableTypeName(channel.Type) {
		return reflectTrySendError("reflect: TrySend of non-chan Value"), nil
	}
	selected, err := waitableTrySendValue(module, channel, value, ctx.batch)
	if err != nil {
		return reflectTrySendError(err.Error()), nil
	}
	return []vmValue{newVMValue("Bool", selected), newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectTrySendError(message string) []vmValue {
	return []vmValue{newVMValue("Bool", false), newVMValue("String", message), newVMValue("Bool", false)}
}

func reflectValueClose(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_close expects 1 argument, got %d", len(args))
	}
	channel, ok, message := reflectCurrentValueArg(args[0], "Close")
	if !ok {
		return []vmValue{newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isWaitableTypeName(channel.Type) {
		return []vmValue{newVMValue("String", "reflect: Close of non-chan Value"), newVMValue("Bool", false)}, nil
	}
	if err := waitableCloseValue(module, channel); err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
}
