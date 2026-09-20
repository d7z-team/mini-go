package runtime

import (
	"errors"
	"time"
)

func timeNow(ctx intrinsicContext, _ []vmValue) ([]vmValue, error) {
	ctx.vm.hostMu.Lock()
	now := ctx.vm.clock.Now()
	ctx.vm.hostMu.Unlock()
	return []vmValue{
		newVMValue("Int64", now.Unix()),
		newVMValue("Int64", int64(now.Nanosecond())),
	}, nil
}

func timeTimerStart(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if ctx.task == nil {
		return nil, errors.New("timer requires an active execution task")
	}
	delay, err := asInt64(args[1])
	if err != nil {
		return nil, err
	}
	period, err := asInt64(args[2])
	if err != nil {
		return nil, err
	}
	if err := ctx.vm.startTimer(ctx.task.scope, ctx.module, args[0], time.Duration(delay), time.Duration(period)); err != nil {
		return nil, err
	}
	return nil, nil
}

func timeTimerStop(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	stopped, err := ctx.vm.stopTimer(ctx.module, args[0])
	if err != nil {
		return nil, err
	}
	return []vmValue{newVMValue("Bool", stopped)}, nil
}
