package runtime

import (
	"errors"
	"fmt"
)

type reflectMakeFuncTarget struct {
	functionType  vmType
	params        []vmType
	results       []vmType
	variadic      bool
	handler       functionRef
	handlerModule *moduleInstance
}

func reflectMakeFunc(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.make_func expects 2 arguments, got %d", len(args))
	}
	info, err := reflectResolvedTypeInfo(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	module := reflectRelationModule(ctx)
	if module == nil {
		return reflectValueError("reflect: MakeFunc requires module context"), nil
	}
	functionType := module.resolvedRuntimeType(info.Key)
	function, ok := functionType.FunctionInfo()
	if !ok {
		return reflectValueError("reflect: call of MakeFunc with non-Func type"), nil
	}
	underlying := functionType.Underlying()
	if !underlying.Valid() {
		return reflectValueError("reflect: MakeFunc requires a valid function type"), nil
	}
	params := make([]vmType, len(function.Signature.Params))
	for index, param := range function.Signature.Params {
		params[index] = vmType{Ref: param.Type, Table: underlying.Table}
	}
	results := make([]vmType, len(function.Signature.Results))
	for index, result := range function.Signature.Results {
		results[index] = vmType{Ref: result, Table: underlying.Table}
	}
	if ctx.vm == nil {
		return reflectValueError("reflect: MakeFunc requires VM context"), nil
	}
	handlerModule, handler, err := ctx.vm.resolveFunctionValue(reflectRelationModule(ctx), args[1])
	if err != nil {
		return reflectValueError("reflect: MakeFunc handler is not callable"), nil
	}
	handlerFunction := handlerModule.executable.Functions[handler.FunctionID]
	if len(handlerFunction.Decl.Signature.Params) != 1 || len(handlerFunction.Decl.Signature.Results) != 1 {
		return reflectValueError("reflect: MakeFunc handler must have type func([]reflect.Value) []reflect.Value"), nil
	}
	handler.exact = handlerModule
	target := reflectMakeFuncTarget{
		functionType: functionType, params: params, results: results, variadic: function.Signature.Variadic,
		handler: handler, handlerModule: handlerModule,
	}
	value := newVMValue(functionType, target)
	out, err := reflectValueSnapshot(intrinsicContext{vm: ctx.vm, module: handlerModule}, value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func (target reflectMakeFuncTarget) callbackRequest(vm *vm, args []vmValue) (*artifactCallbackRequest, error) {
	if vm == nil || target.handlerModule == nil {
		return nil, errors.New("reflect: MakeFunc target has no VM context")
	}
	if len(args) != len(target.params) {
		return nil, fmt.Errorf("reflect: MakeFunc call received %d arguments, want %d", len(args), len(target.params))
	}
	reflected := make([]vmValue, 0, len(args))
	ctx := intrinsicContext{vm: vm, module: target.handlerModule}
	for index, arg := range args {
		value, err := reflectValueSnapshot(ctx, arg)
		if err != nil {
			return nil, fmt.Errorf("reflect: MakeFunc argument %d: %w", index, err)
		}
		reflected = append(reflected, value)
	}
	handlerFunction, ok := target.handlerModule.executable.Functions[target.handler.FunctionID]
	if !ok {
		return nil, fmt.Errorf("reflect: MakeFunc handler %s is missing", target.handler.FunctionID)
	}
	request := &artifactCallbackRequest{
		module: target.handlerModule, functionID: target.handler.FunctionID,
		args:         []vmValue{newSliceValue("Slice<reflect.Value>", reflected)},
		captureSlots: target.handler.captureSlots, resultCount: len(handlerFunction.ResultTypes),
	}
	request.result = reflectCallResult{module: target.handlerModule, makeFunc: true, types: target.results}
	return request, nil
}

// reflectCallResult holds only the metadata needed after a guest call returns.
// Arguments and captures belong to the callee frame, not its completion handler.
type reflectCallResult struct {
	module    *moduleInstance
	qualify   bool
	makeFunc  bool
	types     []vmType
	reflected bool
	ctx       intrinsicContext
}

func (result reflectCallResult) complete(values []vmValue) ([]vmValue, error) {
	if result.makeFunc {
		if len(values) != 1 {
			return nil, fmt.Errorf("reflect: MakeFunc handler returned %d values, want one result slice", len(values))
		}
		reflectedResults, err := reflectValueCallArguments(values[0])
		if err != nil {
			return nil, fmt.Errorf("reflect: MakeFunc handler result: %w", err)
		}
		if len(reflectedResults) != len(result.types) {
			return nil, fmt.Errorf("reflect: wrong return count from function created by MakeFunc: got %d, want %d", len(reflectedResults), len(result.types))
		}
		out := make([]vmValue, 0, len(result.types))
		for index, reflectedResult := range reflectedResults {
			value, err := reflectArgumentRuntimeValue(reflectedResult)
			if err != nil {
				return nil, fmt.Errorf("reflect: MakeFunc result %d: %w", index, err)
			}
			normalized, err := result.module.coerceAssignableValue(value, result.types[index])
			if err != nil {
				return nil, fmt.Errorf("reflect: MakeFunc result %d: %w", index, err)
			}
			out = append(out, normalized)
		}
		values = out
	}
	if result.qualify {
		values = result.module.qualifyValuesForExport(values)
	}
	if result.reflected {
		return reflectValueCallResults(result.ctx, values)
	}
	return values, nil
}

func (machine *executionMachine) reflectCallCompletion(result reflectCallResult, expected int, description string) func(*executionTask, *executionFrame, []vmValue) error {
	return func(task *executionTask, caller *executionFrame, values []vmValue) error {
		values, err := result.complete(values)
		if err == nil && len(values) != expected {
			err = fmt.Errorf("%s %d results, expected %d", description, len(values), expected)
		}
		if err != nil {
			machine.startPanic(task, caller, caller.frame.pc-1, newVMValue("String", err.Error()))
			return nil
		}
		for _, value := range values {
			caller.frame.push(value)
		}
		return nil
	}
}
