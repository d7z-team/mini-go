package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

type reflectMethodTarget struct {
	Receiver vmValue
	Method   TypeMethodInfo
	Module   *moduleInstance
}

type reflectUnboundMethodTarget struct {
	Method TypeMethodInfo
	Module *moduleInstance
}

func reflectValueMethod(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_method expects 2 arguments, got %d", len(args))
	}
	value, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	receiver, ok, err := reflectCurrentValue(value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		return reflectValueError("reflect: Method of invalid Value"), nil
	}
	method, err := reflectMethodInfoFromValue(ctx, args[1])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	signature := method.SignatureType.String()
	if strings.TrimSpace(signature) == "" {
		return reflectValueError("reflect: Method with empty signature"), nil
	}
	targetModule, err := reflectMethodModule(ctx, reflectRelationModule(ctx), method)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	target := newVMValue(signature, reflectMethodTarget{Receiver: receiver, Method: method, Module: targetModule})
	out, err := reflectValueSnapshot(ctx, target)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectValueCall(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("reflect.value_call expects 3 arguments, got %d", len(args))
	}
	value, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueCallError(err.Error()), nil
	}
	reflectedArgs, err := reflectValueCallArguments(args[1])
	if err != nil {
		return reflectValueCallError(err.Error()), nil
	}
	callSlice, ok := args[2].boolValue()
	if !ok || !args[2].Type.Primitive(types.PrimitiveBool) {
		return reflectValueCallError("reflect.value_call expects Bool callSlice flag"), nil
	}
	current, ok, err := reflectCurrentValue(value)
	if err != nil {
		return reflectValueCallError(err.Error()), nil
	}
	if !ok {
		return reflectValueCallError("reflect: Call of invalid Value"), nil
	}
	request, err := reflectCallValue(ctx, current, reflectedArgs, callSlice)
	if err != nil {
		return reflectValueCallError(err.Error()), nil
	}
	request.result.reflected = true
	request.result.ctx = ctx
	return nil, request
}

func reflectValueCallResults(ctx intrinsicContext, results []vmValue) ([]vmValue, error) {
	snapshots := make([]vmValue, 0, len(results))
	for _, result := range results {
		snapshot, err := reflectValueSnapshot(ctx, result)
		if err != nil {
			return reflectValueCallError(err.Error()), nil
		}
		snapshots = append(snapshots, snapshot)
	}
	return []vmValue{newSliceValue("Slice<reflect.Value>", snapshots), newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectCallValue(ctx intrinsicContext, current vmValue, reflectedArgs []vmValue, callSlice bool) (*artifactCallbackRequest, error) {
	if target, ok := current.payload().(reflectMethodTarget); ok {
		return reflectCallMethodTarget(ctx, target, reflectedArgs, callSlice)
	}
	if target, ok := current.payload().(reflectUnboundMethodTarget); ok {
		return reflectCallUnboundMethodTarget(ctx, target, reflectedArgs, callSlice)
	}
	module := reflectRelationModule(ctx)
	if ctx.vm == nil {
		return nil, errors.New("reflect: Call requires VM context")
	}
	if target, ok := current.payload().(reflectMakeFuncTarget); ok {
		callArgs, err := reflectPrepareCallArguments(module, target.handlerModule, target.functionType.String(), target.variadic, reflectedArgs, callSlice)
		if err != nil {
			return nil, err
		}
		if module != nil && module != target.handlerModule {
			callArgs = module.qualifyValuesForArgumentBoundary(callArgs)
		}
		request, err := target.callbackRequest(ctx.vm, callArgs)
		if err != nil {
			return nil, err
		}
		return request, nil
	}
	targetModule, ref, err := ctx.vm.resolveFunctionValue(module, current)
	if err != nil {
		return nil, errors.New("reflect: Call of non-function Value")
	}
	signature := current.Type.String()
	variadic := false
	if fn, ok := targetModule.executable.Functions[ref.FunctionID]; ok {
		// FunctionRef resolves the artifact declaration, which is authoritative
		// even when the source value has a named function type.
		signature = types.FormatSignature(&targetModule.executable.Artifact.TypeTable, fn.Decl.Signature)
		variadic = fn.Decl.Signature.Variadic
	}
	callArgs, err := reflectPrepareCallArguments(module, targetModule, signature, variadic, reflectedArgs, callSlice)
	if err != nil {
		return nil, err
	}
	if targetModule != module && module != nil {
		callArgs = module.qualifyValuesForArgumentBoundary(callArgs)
	}
	function, ok := targetModule.executable.Functions[ref.FunctionID]
	if !ok {
		return nil, fmt.Errorf("reflect: Call target %s is missing", ref.FunctionID)
	}
	request := &artifactCallbackRequest{
		module:       targetModule,
		functionID:   ref.FunctionID,
		args:         callArgs,
		captureSlots: ref.captureSlots,
		resultCount:  len(function.ResultTypes),
	}
	request.result = reflectCallResult{module: targetModule, qualify: targetModule != module}
	return request, nil
}

func reflectCallUnboundMethodTarget(ctx intrinsicContext, target reflectUnboundMethodTarget, reflectedArgs []vmValue, callSlice bool) (*artifactCallbackRequest, error) {
	if len(reflectedArgs) == 0 {
		return nil, errors.New("reflect: Call with too few input arguments")
	}
	if target.Module == nil || ctx.vm == nil {
		return nil, fmt.Errorf("reflect: method %s has no defining module", target.Method.Name)
	}
	receiver, err := reflectArgumentRuntimeValue(reflectedArgs[0])
	if err != nil {
		return nil, err
	}
	currentModule := reflectRelationModule(ctx)
	if currentModule != nil && currentModule != target.Module {
		receiver = currentModule.qualifyValueForArgumentBoundary(receiver)
	}
	receiver, err = target.Module.coerceAssignableValue(receiver, target.Method.ReceiverType)
	if err != nil {
		return nil, fmt.Errorf("reflect: Call argument 0: %w", err)
	}
	signature := target.Method.SignatureType.String()
	callArgs, err := reflectPrepareCallArguments(currentModule, target.Module, signature, target.Method.Variadic, reflectedArgs[1:], callSlice)
	if err != nil {
		return nil, err
	}
	args := append([]vmValue{receiver}, callArgs...)
	function, ok := target.Module.executable.Functions[target.Method.FunctionID]
	if !ok {
		return nil, fmt.Errorf("reflect: method %s target %s is missing", target.Method.Name, target.Method.FunctionID)
	}
	request := &artifactCallbackRequest{
		module: target.Module, functionID: target.Method.FunctionID,
		args: args, resultCount: len(function.ResultTypes),
	}
	request.result = reflectCallResult{module: target.Module, qualify: target.Module != currentModule}
	return request, nil
}

func reflectCallMethodTarget(ctx intrinsicContext, target reflectMethodTarget, reflectedArgs []vmValue, callSlice bool) (*artifactCallbackRequest, error) {
	if strings.TrimSpace(target.Method.FunctionID) == "" {
		return nil, fmt.Errorf("reflect: method %s has no callable function", target.Method.Name)
	}
	if ctx.vm == nil {
		return nil, errors.New("reflect: Call requires VM context")
	}
	currentModule := reflectRelationModule(ctx)
	targetModule := target.Module
	if targetModule == nil {
		return nil, fmt.Errorf("reflect: method %s has no defining module", target.Method.Name)
	}
	signature := target.Method.SignatureType.String()
	callArgs, err := reflectPrepareCallArguments(currentModule, targetModule, signature, target.Method.Variadic, reflectedArgs, callSlice)
	if err != nil {
		return nil, err
	}
	args := append([]vmValue{target.Receiver}, callArgs...)
	if targetModule != currentModule && currentModule != nil {
		args = currentModule.qualifyValuesForArgumentBoundary(args)
	}
	function, ok := targetModule.executable.Functions[target.Method.FunctionID]
	if !ok {
		return nil, fmt.Errorf("reflect: method %s target %s is missing", target.Method.Name, target.Method.FunctionID)
	}
	request := &artifactCallbackRequest{
		module:      targetModule,
		functionID:  target.Method.FunctionID,
		args:        args,
		resultCount: len(function.ResultTypes),
	}
	request.result = reflectCallResult{module: targetModule, qualify: targetModule != currentModule}
	return request, nil
}

func reflectMethodModule(ctx intrinsicContext, current *moduleInstance, method TypeMethodInfo) (*moduleInstance, error) {
	modulePath := strings.TrimSpace(method.ModulePath)
	if modulePath == "" {
		return nil, fmt.Errorf("reflect: method %s has no module", method.Name)
	}
	if current == nil || modulePath == current.modulePath() {
		if current == nil {
			return nil, fmt.Errorf("reflect: method %s has no module", method.Name)
		}
		return current, nil
	}
	module, ok := ctx.vm.moduleRegistry().module(modulePath)
	if !ok {
		return nil, fmt.Errorf("module %q is not loaded", modulePath)
	}
	return module, nil
}

func reflectPrepareCallArguments(current, target *moduleInstance, signature string, variadic bool, reflectedArgs []vmValue, callSlice bool) ([]vmValue, error) {
	params, _, ok := parseFunctionSignatureParts(signature)
	if !ok {
		return nil, fmt.Errorf("reflect: Call of value with invalid function type %s", signature)
	}
	if callSlice && !variadic {
		return nil, errors.New("reflect: CallSlice of non-variadic function")
	}
	fixedCount := len(params)
	if variadic {
		fixedCount--
		if len(params) == 0 {
			return nil, errors.New("reflect: variadic function missing final slice parameter")
		}
	}
	if (!variadic || callSlice) && len(reflectedArgs) != len(params) {
		return nil, errors.New("reflect: Call with too few input arguments")
	}
	if variadic && !callSlice && len(reflectedArgs) < fixedCount {
		return nil, errors.New("reflect: Call with too few input arguments")
	}
	callArgs := make([]vmValue, 0, len(params))
	for i := 0; i < fixedCount; i++ {
		value, err := reflectArgumentRuntimeValue(reflectedArgs[i])
		if err != nil {
			return nil, err
		}
		param := splitCanonicalFunctionParam(params[i])
		if target != nil {
			if current != nil && target != current {
				value = current.qualifyValueForArgumentBoundary(value)
			}
			value, err = target.coerceAssignableValue(value, param)
			if err != nil {
				return nil, fmt.Errorf("reflect: Call argument %d: %w", i, err)
			}
		}
		callArgs = append(callArgs, value)
	}
	if !variadic {
		return callArgs, nil
	}
	finalParam := splitCanonicalFunctionParam(params[len(params)-1])
	if callSlice {
		value, err := reflectArgumentRuntimeValue(reflectedArgs[len(params)-1])
		if err != nil {
			return nil, err
		}
		if target != nil {
			if current != nil && target != current {
				value = current.qualifyValueForArgumentBoundary(value)
			}
			value, err = target.coerceAssignableValue(value, finalParam)
			if err != nil {
				return nil, fmt.Errorf("reflect: CallSlice argument %d: %w", len(params)-1, err)
			}
		}
		callArgs = append(callArgs, value)
		return callArgs, nil
	}
	elemType, ok := runtimeSliceElemText(finalParam)
	if !ok {
		return nil, errors.New("reflect: variadic parameter is not a slice")
	}
	variadicItems := make([]vmValue, 0, len(reflectedArgs)-fixedCount)
	for i := fixedCount; i < len(reflectedArgs); i++ {
		value, err := reflectArgumentRuntimeValue(reflectedArgs[i])
		if err != nil {
			return nil, err
		}
		if target != nil {
			if current != nil && target != current {
				value = current.qualifyValueForArgumentBoundary(value)
			}
			converted, err := target.coerceAssignableValue(value, elemType)
			if err != nil {
				return nil, err
			}
			value = converted
		}
		variadicItems = append(variadicItems, value)
	}
	callArgs = append(callArgs, newSliceValue(finalParam, variadicItems))
	return callArgs, nil
}

func reflectArgumentRuntimeValue(value vmValue) (vmValue, error) {
	fields, err := reflectValuePayload(value)
	if err != nil {
		return vmValue{}, err
	}
	current, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return vmValue{}, err
	}
	if !ok {
		return vmValue{}, errors.New("reflect: zero Value argument")
	}
	return current, nil
}

func reflectValueCallArguments(value vmValue) ([]vmValue, error) {
	items, ok := sliceValues(value)
	if !ok {
		return nil, errors.New("reflect.value_call expects Slice<reflect.Value> arguments")
	}
	out := make([]vmValue, 0, len(items))
	for _, item := range items {
		if _, err := reflectValuePayload(item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func reflectValueCallError(message string) []vmValue {
	return []vmValue{newSliceValue("Slice<reflect.Value>", nil), newVMValue("String", message), newVMValue("Bool", false)}
}
