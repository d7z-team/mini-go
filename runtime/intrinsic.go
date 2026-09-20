package runtime

import (
	"context"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type intrinsicContext struct {
	vm     *vm
	module *moduleInstance
	task   *executionTask
}

func (ctx intrinsicContext) cancellationError() error {
	if ctx.task == nil {
		return nil
	}
	execution := ctx.task.execution
	if execution != nil && execution.cancelRequested.Load() {
		return context.Canceled
	}
	return nil
}

type intrinsicFunc func(intrinsicContext, []vmValue) ([]vmValue, error)

var intrinsicImplementations = map[ir.IntrinsicID]intrinsicFunc{
	"sync.mutex_lock":     mutexLock,
	"sync.mutex_try_lock": mutexTryLock,
	"sync.mutex_unlock":   mutexUnlock,
	"crypto.rand.read":    cryptoRandRead,
	"crypto.sha256.block": sha256Block,
	"math.float64_bits":   mathFloat64bits, "math.float64_from_bits": mathFloat64frombits,
	"math.float32_bits": mathFloat32bits, "math.float32_from_bits": mathFloat32frombits,
	"reflect.type_of": reflectTypeOf, "reflect.type_descriptor": reflectTypeDescriptor,
	"reflect.assignable_to":  reflectAssignableTo,
	"reflect.deep_equal":     reflectDeepEqual,
	"reflect.same_reference": reflectSameReference,
	"reflect.type_field":     reflectTypeField, "reflect.type_method": reflectTypeMethod,
	"reflect.array_of": reflectArrayOf, "reflect.chan_of": reflectChanOf,
	"reflect.func_of": reflectFuncOf, "reflect.struct_of": reflectStructOf,
	"reflect.convertible_to": reflectConvertibleTo, "reflect.implements": reflectImplements,
	"reflect.pointer_to": reflectTypePointerTo, "reflect.slice_of": reflectTypeSliceOf,
	"reflect.map_of": reflectTypeMapOf, "reflect.value_of": reflectValueOf,
	"reflect.value_elem": reflectValueElem, "reflect.value_addr": reflectValueAddr,
	"reflect.value_slice": reflectValueSlice, "reflect.value_slice3": reflectValueSlice3,
	"reflect.value_current": reflectValueCurrent, "reflect.value_convert": reflectValueConvert,
	"reflect.value_equal":  reflectValueEqual,
	"reflect.value_append": reflectValueAppend, "reflect.value_append_slice": reflectValueAppendSlice,
	"reflect.value_copy": reflectValueCopy, "reflect.value_set_len": reflectValueSetLen,
	"reflect.value_set_cap": reflectValueSetCap, "reflect.value_grow": reflectValueGrow,
	"reflect.value_field_by_index": reflectValueFieldByIndex,
	"reflect.value_index":          reflectValueIndex, "reflect.value_set": reflectValueSet,
	"reflect.value_swap": reflectValueSwap, "reflect.value_set_map_index": reflectValueSetMapIndex,
	"reflect.value_method": reflectValueMethod, "reflect.value_call": reflectValueCall,
	"reflect.make_func": reflectMakeFunc, "reflect.value_recv": reflectValueRecv,
	"reflect.make_chan": reflectMakeChan, "reflect.value_send": reflectValueSend,
	"reflect.value_try_recv": reflectValueTryRecv, "reflect.value_try_send": reflectValueTrySend,
	"reflect.value_close": reflectValueClose,
	"reflect.select":      reflectSelect,
	"reflect.zero":        reflectZero, "reflect.new": reflectNew,
	"reflect.make_slice": reflectMakeSlice, "reflect.make_map": reflectMakeMap,
	"time.now":         timeNow,
	"time.timer_start": timeTimerStart,
	"time.timer_stop":  timeTimerStop,
}

func invokeIntrinsic(ctx intrinsicContext, id ir.IntrinsicID, args []vmValue) ([]vmValue, error) {
	if ctx.task != nil {
		defer func() { ctx.task.preparingSelection = nil }()
	}
	descriptor, ok := ir.Intrinsic(id)
	if !ok || len(args) != descriptor.ArgCount {
		return nil, fmt.Errorf("invalid intrinsic call %q", id)
	}
	implementation := intrinsicImplementations[id]
	if implementation == nil {
		return nil, fmt.Errorf("intrinsic %q is not implemented", id)
	}
	return implementation(ctx, args)
}
