package runtime

import (
	"context"
	"errors"
	"fmt"
)

func (i *Instance) Start(entry string, args ...HostValue) (*Execution, error) {
	return i.startEntry(context.Background(), entry, args...)
}

func (i *Instance) startEntry(ctx context.Context, entry string, args ...HostValue) (*Execution, error) {
	if i == nil || i.vm == nil {
		return nil, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return i.start(ctx, false, func(revision *instanceRevision) (int64, error) {
		function, ok := revision.entries[entry]
		if !ok {
			return 0, Error{Err: fmt.Errorf("execution image does not declare entry %q", entry)}
		}
		values, err := i.convertHostArguments(ctx, revision.root, args)
		if err != nil {
			return 0, err
		}
		return i.vm.prepareFunction(function, values)
	})
}

func (i *Instance) convertHostArguments(ctx context.Context, module *moduleInstance, args []HostValue) ([]vmValue, error) {
	values := make([]vmValue, len(args))
	logicalBytes := int64(0)
	for index, arg := range args {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		measured, err := measureHostValue(ctx, arg, i.vm.limits)
		if err != nil {
			return nil, fmt.Errorf("host argument %d: %w", index, err)
		}
		if err := addMeasuredBytes(&logicalBytes, measured); err != nil {
			return nil, fmt.Errorf("host argument %d: %w", index, err)
		}
		value, err := hostValueToVM(ctx, module, arg.Type(), arg)
		if err != nil {
			return nil, fmt.Errorf("host argument %d: %w", index, err)
		}
		values[index] = value
	}
	if err := i.vm.chargeAllocationBytes(logicalBytes); err != nil {
		return nil, err
	}
	return values, nil
}

func (i *Instance) StartEntry(args ...HostValue) (*Execution, error) {
	return i.startDefaultEntry(context.Background(), args...)
}

func (i *Instance) startDefaultEntry(ctx context.Context, args ...HostValue) (*Execution, error) {
	if i == nil || i.vm == nil {
		return nil, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return i.start(ctx, false, func(revision *instanceRevision) (int64, error) {
		if revision.entry == "" {
			return 0, errors.New("program has no default entry")
		}
		values, err := i.convertHostArguments(ctx, revision.root, args)
		if err != nil {
			return 0, err
		}
		return i.vm.prepareFunction(revision.entry, values)
	})
}

// StartMain starts the root package's real main function with program lifecycle semantics.
func (i *Instance) StartMain() (*Execution, error) {
	return i.startMain(context.Background())
}

func (i *Instance) startMain(ctx context.Context) (*Execution, error) {
	if i == nil || i.vm == nil {
		return nil, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return i.start(ctx, true, func(*instanceRevision) (int64, error) {
		return i.vm.prepareProgramEntry()
	})
}

func (i *Instance) start(ctx context.Context, program bool, prepare func(*instanceRevision) (int64, error)) (*Execution, error) {
	if i == nil || i.vm == nil {
		return nil, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := i.vm.enterOwnerContext(ctx); err != nil {
		return nil, err
	}
	defer i.vm.leaveOwner()
	if !i.isOpen() {
		return nil, i.unavailableError()
	}
	if i.active.Load() != nil || i.vm.machine != nil && i.vm.machine.foreground != nil {
		return nil, errors.New("instance already has an active execution")
	}
	revision := i.vm.revision.Load()
	if revision == nil {
		return nil, errors.New("instance has no active revision")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scopeID, err := prepare(revision)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		if i.vm.machine != nil {
			i.vm.machine.cancelScope(i.vm.machine.scope(scopeID), err)
		}
		i.vm.finishForeground()
		return nil, err
	}
	execution := newExecution(i, i.vm.profileOptions)
	execution.scopeID = scopeID
	execution.program = program
	if i.vm.machine != nil && i.vm.machine.foreground != nil {
		i.vm.machine.foreground.execution = execution
		i.vm.machine.attachExecution(scopeID, execution)
	}
	i.active.Store(execution)
	return execution, nil
}

func (i *Instance) Call(ctx context.Context, entry string, args ...HostValue) (RunResult, error) {
	execution, err := i.startEntry(ctx, entry, args...)
	if err != nil {
		return RunResult{}, err
	}
	return execution.Wait(ctx)
}

func (i *Instance) CallEntry(ctx context.Context, args ...HostValue) (RunResult, error) {
	execution, err := i.startDefaultEntry(ctx, args...)
	if err != nil {
		return RunResult{}, err
	}
	return execution.Wait(ctx)
}

// CallMain runs the root package's real main function to program completion.
func (i *Instance) CallMain(ctx context.Context) (RunResult, error) {
	execution, err := i.startMain(ctx)
	if err != nil {
		return RunResult{}, err
	}
	return execution.Wait(ctx)
}
