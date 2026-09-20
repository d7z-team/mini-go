package bytecode

import "github.com/d7z-team/mini-go/compiler/types"

// IntrinsicID identifies VM-owned behavior that cannot be implemented by
// ordinary Mini-Go source. IDs are part of the bytecode contract.
type IntrinsicID string

// IntrinsicSchema identifies the descriptor and execution semantics used by
// call_intrinsic instructions.
const IntrinsicSchema = "minigo.intrinsic.v5"

// IntrinsicEffect describes how an intrinsic interacts with VM state.
type IntrinsicEffect uint8

const (
	IntrinsicPure IntrinsicEffect = iota
	IntrinsicMutatesVM
	IntrinsicMayCallGuest
	IntrinsicMayBlock
	IntrinsicReadsRuntime
)

// IntrinsicDescriptor is the shared compiler/runtime contract for one
// intrinsic. SourceFunction is matched only in SourceModule.
type IntrinsicDescriptor struct {
	ID             IntrinsicID
	SourceModule   string
	SourceFunction string
	Signature      string
	DynamicResult  types.TypeKey
	ArgCount       int
	ResultCount    int
	Effect         IntrinsicEffect
}

var reflectRuntimeType = types.TypeKey{ModulePath: "reflect", DeclID: "runtimeType"}

var intrinsicDescriptors = []IntrinsicDescriptor{
	{ID: "sync.mutex_lock", SourceModule: "sync", SourceFunction: "runtimeMutexLock", Signature: "function(Ptr<Waitable<Bool>>)", ArgCount: 1, Effect: IntrinsicMayBlock},
	{ID: "sync.mutex_try_lock", SourceModule: "sync", SourceFunction: "runtimeMutexTryLock", Signature: "function(Ptr<Waitable<Bool>>) Bool", ArgCount: 1, ResultCount: 1, Effect: IntrinsicMutatesVM},
	{ID: "sync.mutex_unlock", SourceModule: "sync", SourceFunction: "runtimeMutexUnlock", Signature: "function(Ptr<Waitable<Bool>>)", ArgCount: 1, Effect: IntrinsicMutatesVM},
	{ID: "crypto.rand.read", SourceModule: "crypto/rand", SourceFunction: "runtimeRead", Signature: "function(Slice<Uint8>) tuple(Int, String, Bool)", ArgCount: 1, ResultCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "crypto.sha256.block", SourceModule: "crypto/sha256", SourceFunction: "runtimeBlock", Signature: "function(Array<8, Uint32>, Slice<Uint8>) Array<8, Uint32>", ArgCount: 2, ResultCount: 1, Effect: IntrinsicPure},
	{ID: "ffi.call", SourceModule: "ffi", SourceFunction: "runtimeCall", Signature: "function(String, Slice<Uint8>) tuple(Slice<Uint8>, String, Int)", ArgCount: 2, ResultCount: 3, Effect: IntrinsicMayBlock},
	{ID: "math.float64_bits", SourceModule: "math", SourceFunction: "runtimeFloat64bits", Signature: "function(Float64) Uint64", ArgCount: 1, ResultCount: 1, Effect: IntrinsicPure},
	{ID: "math.float64_from_bits", SourceModule: "math", SourceFunction: "runtimeFloat64frombits", Signature: "function(Uint64) Float64", ArgCount: 1, ResultCount: 1, Effect: IntrinsicPure},
	{ID: "math.float32_bits", SourceModule: "math", SourceFunction: "runtimeFloat32bits", Signature: "function(Float32) Uint32", ArgCount: 1, ResultCount: 1, Effect: IntrinsicPure},
	{ID: "math.float32_from_bits", SourceModule: "math", SourceFunction: "runtimeFloat32frombits", Signature: "function(Uint32) Float32", ArgCount: 1, ResultCount: 1, Effect: IntrinsicPure},
	{ID: "reflect.type_of", SourceModule: "reflect", SourceFunction: "runtimeTypeOf", Signature: "function(Any) reflect.Type", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 1},
	{ID: "reflect.type_descriptor", SourceModule: "reflect", SourceFunction: "runtimeTypeDescriptor", Signature: "function(reflect.Type) tuple(reflect.runtimeTypeData, String, Bool)", ArgCount: 1, ResultCount: 3},
	{ID: "reflect.deep_equal", SourceModule: "reflect", SourceFunction: "runtimeDeepEqual", Signature: "function(Any, Any) Bool", ArgCount: 2, ResultCount: 1},
	{ID: "reflect.same_reference", SourceModule: "reflect", SourceFunction: "runtimeSameReference", Signature: "function(reflect.Value, reflect.Value) Bool", ArgCount: 2, ResultCount: 1},
	{ID: "reflect.type_field", SourceModule: "reflect", SourceFunction: "runtimeTypeField", Signature: "function(reflect.Type, Int) tuple(reflect.StructField, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.type_method", SourceModule: "reflect", SourceFunction: "runtimeTypeMethod", Signature: "function(reflect.Type, Int) tuple(reflect.Method, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.array_of", SourceModule: "reflect", SourceFunction: "runtimeArrayOf", Signature: "function(Int, reflect.Type) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.chan_of", SourceModule: "reflect", SourceFunction: "runtimeChanOf", Signature: "function(reflect.ChanDir, reflect.Type) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.func_of", SourceModule: "reflect", SourceFunction: "runtimeFuncOf", Signature: "function(Slice<reflect.Type>, Slice<reflect.Type>, Bool) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 3, ResultCount: 3},
	{ID: "reflect.struct_of", SourceModule: "reflect", SourceFunction: "runtimeStructOf", Signature: "function(Slice<reflect.StructField>) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.assignable_to", SourceModule: "reflect", SourceFunction: "runtimeAssignableTo", Signature: "function(reflect.Type, reflect.Type) Bool", ArgCount: 2, ResultCount: 1},
	{ID: "reflect.convertible_to", SourceModule: "reflect", SourceFunction: "runtimeConvertibleTo", Signature: "function(reflect.Type, reflect.Type) Bool", ArgCount: 2, ResultCount: 1},
	{ID: "reflect.implements", SourceModule: "reflect", SourceFunction: "runtimeImplements", Signature: "function(reflect.Type, reflect.Type) Bool", ArgCount: 2, ResultCount: 1},
	{ID: "reflect.pointer_to", SourceModule: "reflect", SourceFunction: "runtimePointerTo", Signature: "function(reflect.Type) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.slice_of", SourceModule: "reflect", SourceFunction: "runtimeSliceOf", Signature: "function(reflect.Type) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.map_of", SourceModule: "reflect", SourceFunction: "runtimeMapOf", Signature: "function(reflect.Type, reflect.Type) tuple(reflect.Type, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_of", SourceModule: "reflect", SourceFunction: "runtimeValueOf", Signature: "function(Any) reflect.Value", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 1},
	{ID: "reflect.value_elem", SourceModule: "reflect", SourceFunction: "runtimeValueElem", Signature: "function(reflect.Value) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.value_addr", SourceModule: "reflect", SourceFunction: "runtimeValueAddr", Signature: "function(reflect.Value) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.value_slice", SourceModule: "reflect", SourceFunction: "runtimeValueSlice", Signature: "function(reflect.Value, Int, Int) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 3, ResultCount: 3},
	{ID: "reflect.value_slice3", SourceModule: "reflect", SourceFunction: "runtimeValueSlice3", Signature: "function(reflect.Value, Int, Int, Int) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 4, ResultCount: 3},
	{ID: "reflect.value_current", SourceModule: "reflect", SourceFunction: "runtimeValueCurrent", Signature: "function(reflect.Value) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.value_equal", SourceModule: "reflect", SourceFunction: "runtimeValueEqual", Signature: "function(reflect.Value, reflect.Value) tuple(Bool, String, Bool)", ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_convert", SourceModule: "reflect", SourceFunction: "runtimeValueConvert", Signature: "function(reflect.Value, reflect.Type) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_append", SourceModule: "reflect", SourceFunction: "runtimeValueAppend", Signature: "function(reflect.Value, Slice<reflect.Value>) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_append_slice", SourceModule: "reflect", SourceFunction: "runtimeValueAppendSlice", Signature: "function(reflect.Value, reflect.Value) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_copy", SourceModule: "reflect", SourceFunction: "runtimeValueCopy", Signature: "function(reflect.Value, reflect.Value) tuple(Int, String, Bool)", ArgCount: 2, ResultCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_set_len", SourceModule: "reflect", SourceFunction: "runtimeValueSetLen", Signature: "function(reflect.Value, Int) tuple(String, Bool)", ArgCount: 2, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_set_cap", SourceModule: "reflect", SourceFunction: "runtimeValueSetCap", Signature: "function(reflect.Value, Int) tuple(String, Bool)", ArgCount: 2, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_grow", SourceModule: "reflect", SourceFunction: "runtimeValueGrow", Signature: "function(reflect.Value, Int) tuple(String, Bool)", ArgCount: 2, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_field_by_index", SourceModule: "reflect", SourceFunction: "runtimeValueFieldByIndex", Signature: "function(reflect.Value, Slice<Int>) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_index", SourceModule: "reflect", SourceFunction: "runtimeValueIndex", Signature: "function(reflect.Value, Int) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_set", SourceModule: "reflect", SourceFunction: "runtimeValueSet", Signature: "function(reflect.Value, reflect.Value) tuple(String, Bool)", ArgCount: 2, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_swap", SourceModule: "reflect", SourceFunction: "runtimeValueSwap", Signature: "function(reflect.Value, Int, Int) tuple(String, Bool)", ArgCount: 3, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_set_map_index", SourceModule: "reflect", SourceFunction: "runtimeValueSetMapIndex", Signature: "function(reflect.Value, reflect.Value, reflect.Value) tuple(String, Bool)", ArgCount: 3, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_method", SourceModule: "reflect", SourceFunction: "runtimeValueMethod", Signature: "function(reflect.Value, reflect.Method) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_call", SourceModule: "reflect", SourceFunction: "runtimeValueCall", Signature: "function(reflect.Value, Slice<reflect.Value>, Bool) tuple(Slice<reflect.Value>, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 3, ResultCount: 3, Effect: IntrinsicMayCallGuest},
	{ID: "reflect.make_func", SourceModule: "reflect", SourceFunction: "runtimeMakeFunc", Signature: "function(reflect.Type, function(Slice<reflect.Value>) Slice<reflect.Value>) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3},
	{ID: "reflect.value_recv", SourceModule: "reflect", SourceFunction: "runtimeValueRecv", Signature: "function(reflect.Value) tuple(reflect.Value, Bool, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 4, Effect: IntrinsicMayBlock},
	{ID: "reflect.make_chan", SourceModule: "reflect", SourceFunction: "runtimeMakeChan", Signature: "function(reflect.Type, Int) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 2, ResultCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_send", SourceModule: "reflect", SourceFunction: "runtimeValueSend", Signature: "function(reflect.Value, reflect.Value) tuple(String, Bool)", ArgCount: 2, ResultCount: 2, Effect: IntrinsicMayBlock},
	{ID: "reflect.value_try_recv", SourceModule: "reflect", SourceFunction: "runtimeValueTryRecv", Signature: "function(reflect.Value) tuple(reflect.Value, Bool, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 4, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_try_send", SourceModule: "reflect", SourceFunction: "runtimeValueTrySend", Signature: "function(reflect.Value, reflect.Value) tuple(Bool, String, Bool)", ArgCount: 2, ResultCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "reflect.value_close", SourceModule: "reflect", SourceFunction: "runtimeValueClose", Signature: "function(reflect.Value) tuple(String, Bool)", ArgCount: 1, ResultCount: 2, Effect: IntrinsicMutatesVM},
	{ID: "reflect.select", SourceModule: "reflect", SourceFunction: "runtimeSelect", Signature: "function(Slice<reflect.SelectCase>) tuple(Int, reflect.Value, Bool, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 5, Effect: IntrinsicMayBlock},
	{ID: "reflect.zero", SourceModule: "reflect", SourceFunction: "runtimeZero", Signature: "function(reflect.Type) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.new", SourceModule: "reflect", SourceFunction: "runtimeNew", Signature: "function(reflect.Type) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "reflect.make_slice", SourceModule: "reflect", SourceFunction: "runtimeMakeSlice", Signature: "function(reflect.Type, Int, Int) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 3, ResultCount: 3},
	{ID: "reflect.make_map", SourceModule: "reflect", SourceFunction: "runtimeMakeMap", Signature: "function(reflect.Type) tuple(reflect.Value, String, Bool)", DynamicResult: reflectRuntimeType, ArgCount: 1, ResultCount: 3},
	{ID: "time.now", SourceModule: "time", SourceFunction: "runtimeNow", Signature: "function() tuple(Int64, Int64)", ResultCount: 2, Effect: IntrinsicReadsRuntime},
	{ID: "time.timer_start", SourceModule: "time", SourceFunction: "runtimeTimerStart", Signature: "function(Waitable<Bool>, Int64, Int64)", ArgCount: 3, Effect: IntrinsicMutatesVM},
	{ID: "time.timer_stop", SourceModule: "time", SourceFunction: "runtimeTimerStop", Signature: "function(Waitable<Bool>) Bool", ArgCount: 1, ResultCount: 1, Effect: IntrinsicMutatesVM},
}

var intrinsicsByID, intrinsicsBySource = indexIntrinsics()

func indexIntrinsics() (map[IntrinsicID]IntrinsicDescriptor, map[string]IntrinsicDescriptor) {
	byID := make(map[IntrinsicID]IntrinsicDescriptor, len(intrinsicDescriptors))
	bySource := make(map[string]IntrinsicDescriptor, len(intrinsicDescriptors))
	for _, descriptor := range intrinsicDescriptors {
		byID[descriptor.ID] = descriptor
		bySource[descriptor.SourceModule+"\x00"+descriptor.SourceFunction] = descriptor
	}
	return byID, bySource
}

func Intrinsic(id IntrinsicID) (IntrinsicDescriptor, bool) {
	descriptor, ok := intrinsicsByID[id]
	return descriptor, ok
}

func IntrinsicForSource(modulePath, function string) (IntrinsicDescriptor, bool) {
	descriptor, ok := intrinsicsBySource[modulePath+"\x00"+function]
	return descriptor, ok
}
