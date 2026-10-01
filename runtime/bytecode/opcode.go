package bytecode

// Opcode is the numeric operation identity within the current ISA.
type Opcode uint16

const (
	OpInvalid Opcode = iota
	OpTypeDispatch
	OpSelect
	OpConst
	OpZero
	OpPop
	OpUnary
	OpBinary
	OpLoadLocal
	OpStoreLocal
	OpLoadUpvalue
	OpStoreUpvalue
	OpLoadGlobal
	OpStoreGlobal
	OpLabel
	OpJump
	OpJumpIf
	OpReturn
	OpPanic
	OpRecover
	OpDeferPush
	OpCallValue
	OpCallDirect
	OpTailCallDirect
	OpCallInterface
	OpMakeClosure
	OpMakeSequence
	OpMakeMap
	OpMakeStruct
	OpMakeSlice
	OpMakeWaitable
	OpLoadIndex
	OpLoadIndexOK
	OpStringRuneAt
	OpStringNextRuneIndex
	OpSlice
	OpLen
	OpCap
	OpAppend
	OpDelete
	OpClear
	OpCopy
	OpMapKeys
	OpMapIterInit
	OpMapIterNext
	OpMapIterClose
	OpLoadField
	OpStoreIndex
	OpStoreField
	OpTypeAssert
	OpTypeAssertOK
	OpConvert
	OpAddressOf
	OpLoadIndirect
	OpStoreIndirect
	OpWaitableSend
	OpWaitableRecv
	OpWaitableRecvOK
	OpWaitableCanRecv
	OpWaitableTryRecv
	OpWaitableTrySend
	OpWaitableCanSend
	OpWaitableClose
	OpInitModule
	OpLoadExport
	OpSpawn
	OpCallFFI
	OpCallIntrinsic
	OpCompareBranch
	OpGetPath
)

func IsKnownOpcode(op Opcode) bool { return op > OpInvalid && int(op) <= len(opcodeSpecs) }

func (op Opcode) String() string {
	if !IsKnownOpcode(op) {
		return ""
	}
	return opcodeSpecs[int(op)-1].Op
}

// ParseOpcode decodes the textual operation name at an image boundary.
func ParseOpcode(name string) (Opcode, bool) { op, ok := knownOpcodes[name]; return op, ok }
