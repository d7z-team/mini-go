package bytecode

type Opcode string

const (
	OpConst        Opcode = "const"
	OpZero         Opcode = "zero"
	OpPop          Opcode = "pop"
	OpUnary        Opcode = "unary"
	OpBinary       Opcode = "binary"
	OpLoadLocal    Opcode = "load_local"
	OpStoreLocal   Opcode = "store_local"
	OpLoadUpvalue  Opcode = "load_upvalue"
	OpStoreUpvalue Opcode = "store_upvalue"
	OpLoadGlobal   Opcode = "load_global"
	OpStoreGlobal  Opcode = "store_global"

	OpLabel     Opcode = "label"
	OpJump      Opcode = "jump"
	OpJumpIf    Opcode = "jump_if"
	OpReturn    Opcode = "return"
	OpPanic     Opcode = "panic"
	OpRecover   Opcode = "recover"
	OpDeferPush Opcode = "defer_push"

	OpCallValue      Opcode = "call_value"
	OpCallDirect     Opcode = "call_direct"
	OpTailCallDirect Opcode = "tail_call_direct"
	OpCallInterface  Opcode = "call_interface"
	OpMakeClosure    Opcode = "make_closure"

	OpMakeSequence        Opcode = "make_sequence"
	OpMakeMap             Opcode = "make_map"
	OpMakeStruct          Opcode = "make_struct"
	OpMakeSlice           Opcode = "make_slice"
	OpMakeWaitable        Opcode = "make_waitable"
	OpLoadIndex           Opcode = "load_index"
	OpLoadIndexOK         Opcode = "load_index_ok"
	OpStringRuneAt        Opcode = "string_rune_at"
	OpStringNextRuneIndex Opcode = "string_next_rune_index"
	OpSlice               Opcode = "slice"
	OpLen                 Opcode = "len"
	OpCap                 Opcode = "cap"
	OpAppend              Opcode = "append"
	OpDelete              Opcode = "delete"
	OpClear               Opcode = "clear"
	OpCopy                Opcode = "copy"
	OpMapKeys             Opcode = "map_keys"
	OpMapIterInit         Opcode = "map_iter_init"
	OpMapIterNext         Opcode = "map_iter_next"
	OpMapIterClose        Opcode = "map_iter_close"
	OpLoadField           Opcode = "load_field"
	OpStoreIndex          Opcode = "store_index"
	OpStoreField          Opcode = "store_field"

	OpTypeAssert    Opcode = "type_assert"
	OpTypeAssertOK  Opcode = "type_assert_ok"
	OpConvert       Opcode = "convert"
	OpAddressOf     Opcode = "address_of"
	OpLoadIndirect  Opcode = "load_indirect"
	OpStoreIndirect Opcode = "store_indirect"

	OpWaitableSend    Opcode = "waitable_send"
	OpSelect          Opcode = "select"
	OpWaitableRecv    Opcode = "waitable_recv"
	OpWaitableRecvOK  Opcode = "waitable_recv_ok"
	OpWaitableCanRecv Opcode = "waitable_can_recv"
	OpWaitableTryRecv Opcode = "waitable_try_recv"
	OpWaitableTrySend Opcode = "waitable_try_send"
	OpWaitableCanSend Opcode = "waitable_can_send"
	OpWaitableClose   Opcode = "waitable_close"

	OpInitModule Opcode = "init_module"
	OpLoadExport Opcode = "load_export"

	OpSpawn Opcode = "spawn"

	OpCallFFI       Opcode = "call_ffi"
	OpCallIntrinsic Opcode = "call_intrinsic"
)

func IsKnownOpcode(op string) bool {
	_, ok := knownOpcodes[op]
	return ok
}
