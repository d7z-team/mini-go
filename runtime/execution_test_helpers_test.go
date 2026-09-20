package runtime

import (
	"context"
	"errors"
)

func startTestExecution(machine *vm, export string, args ...vmValue) (*Execution, error) {
	if machine == nil {
		return nil, errors.New("nil VM")
	}
	instance := &Instance{vm: machine, done: make(chan struct{})}
	execution, err := instance.start(context.Background(), false, func(revision *instanceRevision) (int64, error) {
		if revision.root != nil && revision.root.executable != nil {
			if declaration, ok := revision.root.executable.Exports[export]; ok && declaration.Kind == "function" {
				export = declaration.ID
			}
		}
		return machine.prepareFunction(export, args)
	})
	if err != nil {
		return nil, err
	}
	for execution.state == ExecutionRunning {
		if _, err := execution.Poll(); err != nil {
			break
		}
	}
	return execution, nil
}

func runTestModuleExport(machine *vm, name string, args ...vmValue) (vmResult, error) {
	execution, err := startTestExecution(machine, name, args...)
	if err != nil {
		return vmResult{}, err
	}
	return execution.result, execution.err
}
