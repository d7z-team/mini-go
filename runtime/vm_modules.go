package runtime

import (
	"errors"
	"fmt"
	"strings"
)

func loadInitializedExport(module *moduleInstance, exportName string) (vmValue, error) {
	if module == nil || module.executable == nil {
		return vmValue{}, errors.New("nil module")
	}
	modulePath := module.executable.Artifact.Module.Path
	export, ok := module.executable.Exports[exportName]
	if !ok {
		return vmValue{}, fmt.Errorf("module %q missing export %q", modulePath, exportName)
	}
	switch export.Kind {
	case "function":
		return newVMValue("Function", functionRef{ModulePath: modulePath, FunctionID: export.ID}), nil
	case "const":
		value, ok, err := module.constantValue(export.ID)
		if err != nil {
			return vmValue{}, err
		}
		if !ok {
			return vmValue{}, fmt.Errorf("module %q missing constant %q", modulePath, export.ID)
		}
		return module.qualifyValueForExport(module.cloneValueForStore(value)), nil
	case "global":
		slot, ok := module.state.globals[export.ID]
		if !ok {
			return vmValue{}, fmt.Errorf("module %q missing global %q", modulePath, export.ID)
		}
		return module.qualifyValueForExport(slot.load()), nil
	default:
		return vmValue{}, fmt.Errorf("module export %q is %s, not loadable yet", exportName, export.Kind)
	}
}

func (vm *vm) directCallModule(current *moduleInstance, modulePath string) (*moduleInstance, error) {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" || modulePath == current.executable.Artifact.Module.Path {
		return current, nil
	}
	if current.registry == nil {
		return nil, fmt.Errorf("module %q has no revision registry", current.modulePath())
	}
	module, ok := current.registry.module(modulePath)
	if !ok {
		return nil, fmt.Errorf("module %q is not loaded", modulePath)
	}
	return module, nil
}

func (vm *vm) moduleForFunctionRef(current *moduleInstance, ref functionRef) (*moduleInstance, error) {
	if ref.exact != nil {
		return ref.exact, nil
	}
	modulePath := strings.TrimSpace(ref.ModulePath)
	if modulePath == "" {
		modulePath = current.modulePath()
	}
	module, ok := vm.moduleRegistry().module(modulePath)
	if !ok {
		return nil, fmt.Errorf("module %q is not loaded", ref.ModulePath)
	}
	return module, nil
}

func (vm *vm) resolveFunctionValue(current *moduleInstance, value vmValue) (*moduleInstance, functionRef, error) {
	if value.Data == nil && current.isFunctionType(value.Type) {
		return nil, functionRef{}, errNilFunctionCall
	}
	ref, ok := value.Data.(functionRef)
	if !ok {
		return nil, functionRef{}, fmt.Errorf("call expects Function value, got %s", value.Type)
	}
	if strings.TrimSpace(ref.FunctionID) == "" {
		return nil, functionRef{}, errNilFunctionCall
	}
	target, err := vm.moduleForFunctionRef(current, ref)
	if err != nil {
		return nil, functionRef{}, err
	}
	if _, ok := target.executable.Functions[ref.FunctionID]; !ok {
		return nil, functionRef{}, fmt.Errorf("unknown function %q", ref.FunctionID)
	}
	return target, ref, nil
}
