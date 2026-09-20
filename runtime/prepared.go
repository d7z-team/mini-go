package runtime

import (
	"encoding/json"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type preparedOpcode uint8

const (
	preparedConst preparedOpcode = iota
	preparedZero
	preparedPop
	preparedUnary
	preparedBinary
	preparedLoadLocal
	preparedStoreLocal
	preparedLoadUpvalue
	preparedStoreUpvalue
	preparedLoadGlobal
	preparedStoreGlobal
	preparedJump
	preparedJumpIf
	preparedReturn
	preparedPanic
	preparedRecover
	preparedDeferPush
	preparedCallValue
	preparedCallDirect
	preparedTailCallDirect
	preparedCallInterface
	preparedMakeClosure
	preparedMakeSequence
	preparedMakeMap
	preparedMakeStruct
	preparedMakeSlice
	preparedMakeWaitable
	preparedLoadIndex
	preparedLoadIndexOK
	preparedStringRuneAt
	preparedStringNextRuneIndex
	preparedSlice
	preparedLen
	preparedCap
	preparedAppend
	preparedDelete
	preparedClear
	preparedCopy
	preparedMapKeys
	preparedMapIterInit
	preparedMapIterNext
	preparedMapIterClose
	preparedLoadField
	preparedStoreIndex
	preparedStoreField
	preparedTypeAssert
	preparedTypeAssertOK
	preparedConvert
	preparedAddressOf
	preparedLoadIndirect
	preparedStoreIndirect
	preparedWaitableSend
	preparedWaitableRecv
	preparedWaitableRecvOK
	preparedWaitableCanRecv
	preparedWaitableTryRecv
	preparedWaitableTrySend
	preparedWaitableCanSend
	preparedWaitableClose
	preparedInitModule
	preparedLoadExport
	preparedSpawn
	preparedCallFFI
	preparedCallIntrinsic
	preparedSelect
)

var preparedOpcodes = map[string]preparedOpcode{
	string(ir.OpSelect):      preparedSelect,
	string(ir.OpMapIterInit): preparedMapIterInit, string(ir.OpMapIterNext): preparedMapIterNext, string(ir.OpMapIterClose): preparedMapIterClose,
	string(ir.OpConst): preparedConst, string(ir.OpZero): preparedZero, string(ir.OpPop): preparedPop,
	string(ir.OpUnary): preparedUnary, string(ir.OpBinary): preparedBinary,
	string(ir.OpLoadLocal): preparedLoadLocal, string(ir.OpStoreLocal): preparedStoreLocal,
	string(ir.OpLoadUpvalue): preparedLoadUpvalue, string(ir.OpStoreUpvalue): preparedStoreUpvalue,
	string(ir.OpLoadGlobal): preparedLoadGlobal, string(ir.OpStoreGlobal): preparedStoreGlobal,
	string(ir.OpJump): preparedJump, string(ir.OpJumpIf): preparedJumpIf,
	string(ir.OpReturn): preparedReturn, string(ir.OpPanic): preparedPanic,
	string(ir.OpRecover): preparedRecover, string(ir.OpDeferPush): preparedDeferPush,
	string(ir.OpCallValue):  preparedCallValue,
	string(ir.OpCallDirect): preparedCallDirect, string(ir.OpCallInterface): preparedCallInterface,
	string(ir.OpTailCallDirect): preparedTailCallDirect,
	string(ir.OpMakeClosure):    preparedMakeClosure, string(ir.OpMakeSequence): preparedMakeSequence,
	string(ir.OpMakeMap): preparedMakeMap, string(ir.OpMakeStruct): preparedMakeStruct,
	string(ir.OpMakeSlice): preparedMakeSlice, string(ir.OpMakeWaitable): preparedMakeWaitable,
	string(ir.OpLoadIndex): preparedLoadIndex, string(ir.OpLoadIndexOK): preparedLoadIndexOK,
	string(ir.OpStringRuneAt): preparedStringRuneAt, string(ir.OpStringNextRuneIndex): preparedStringNextRuneIndex,
	string(ir.OpSlice): preparedSlice, string(ir.OpLen): preparedLen, string(ir.OpCap): preparedCap,
	string(ir.OpAppend): preparedAppend, string(ir.OpDelete): preparedDelete, string(ir.OpClear): preparedClear,
	string(ir.OpCopy): preparedCopy, string(ir.OpMapKeys): preparedMapKeys, string(ir.OpLoadField): preparedLoadField,
	string(ir.OpStoreIndex): preparedStoreIndex, string(ir.OpStoreField): preparedStoreField,
	string(ir.OpTypeAssert): preparedTypeAssert, string(ir.OpTypeAssertOK): preparedTypeAssertOK,
	string(ir.OpConvert): preparedConvert, string(ir.OpAddressOf): preparedAddressOf, string(ir.OpLoadIndirect): preparedLoadIndirect,
	string(ir.OpStoreIndirect): preparedStoreIndirect, string(ir.OpWaitableSend): preparedWaitableSend,
	string(ir.OpWaitableRecv): preparedWaitableRecv, string(ir.OpWaitableRecvOK): preparedWaitableRecvOK,
	string(ir.OpWaitableCanRecv): preparedWaitableCanRecv, string(ir.OpWaitableTryRecv): preparedWaitableTryRecv,
	string(ir.OpWaitableTrySend): preparedWaitableTrySend, string(ir.OpWaitableCanSend): preparedWaitableCanSend,
	string(ir.OpWaitableClose): preparedWaitableClose, string(ir.OpInitModule): preparedInitModule,
	string(ir.OpLoadExport): preparedLoadExport, string(ir.OpSpawn): preparedSpawn,
	string(ir.OpCallFFI):       preparedCallFFI,
	string(ir.OpCallIntrinsic): preparedCallIntrinsic,
}

type preparedOperator uint8

const (
	operatorInvalid preparedOperator = iota
	operatorNot
	operatorAdd
	operatorSub
	operatorMul
	operatorDiv
	operatorMod
	operatorBitAnd
	operatorBitOr
	operatorBitXor
	operatorBitClear
	operatorShiftLeft
	operatorShiftRight
	operatorComplex
	operatorReal
	operatorImag
	operatorEqual
	operatorNotEqual
	operatorLess
	operatorGreater
	operatorLessEqual
	operatorGreaterEqual
	operatorAnd
	operatorOr
)

func parsePreparedOperator(value string) (preparedOperator, bool) {
	switch value {
	case "!":
		return operatorNot, true
	case "+":
		return operatorAdd, true
	case "-":
		return operatorSub, true
	case "*":
		return operatorMul, true
	case "/":
		return operatorDiv, true
	case "%":
		return operatorMod, true
	case "&":
		return operatorBitAnd, true
	case "|":
		return operatorBitOr, true
	case "^":
		return operatorBitXor, true
	case "&^":
		return operatorBitClear, true
	case "<<":
		return operatorShiftLeft, true
	case ">>":
		return operatorShiftRight, true
	case "complex":
		return operatorComplex, true
	case "real":
		return operatorReal, true
	case "imag":
		return operatorImag, true
	case "==":
		return operatorEqual, true
	case "!=":
		return operatorNotEqual, true
	case "<":
		return operatorLess, true
	case ">":
		return operatorGreater, true
	case "<=":
		return operatorLessEqual, true
	case ">=":
		return operatorGreaterEqual, true
	case "&&":
		return operatorAnd, true
	case "||":
		return operatorOr, true
	default:
		return operatorInvalid, false
	}
}

func (operator preparedOperator) String() string {
	return [...]string{"", "!", "+", "-", "*", "/", "%", "&", "|", "^", "&^", "<<", ">>", "complex", "real", "imag", "==", "!=", "<", ">", "<=", ">=", "&&", "||"}[operator]
}

type preparedInstruction struct {
	op        preparedOpcode
	control   bool
	canonical ir.Instruction

	constant      *ir.ConstPayload
	typeOperand   *ir.TypePayload
	operator      preparedOperator
	makeSequence  *ir.MakeSequencePayload
	makeMap       *ir.MakeMapPayload
	makeStruct    *ir.MakeStructPayload
	makeSlice     *ir.MakeSlicePayload
	makeWaitable  *ir.MakeWaitablePayload
	count         *ir.CountPayload
	field         *ir.FieldPayload
	export        *ir.ExportPayload
	initModule    *ir.InitModulePayload
	local         *ir.LocalPayload
	upvalue       *ir.UpvaluePayload
	global        *ir.GlobalPayload
	address       *ir.AddressPayload
	jump          *ir.JumpPayload
	call          *ir.CallPayload
	interfaceCall *ir.CallInterfacePayload
	closure       *ir.ClosurePayload
	returnValue   *ir.ReturnPayload
	deferValue    *ir.DeferPayload
	callFFI       *ir.CallFFIPayload
	callIntrinsic *ir.CallIntrinsicPayload
	selection     *ir.SelectPayload

	constantIndex     int
	localIndex        int
	upvalueIndex      int
	globalIndex       int
	jumpPC            int
	runtimeType       vmType
	typeVariadic      bool
	structSchema      *structSchema
	callModule        string
	callModuleIndex   int
	callFunctionIndex int
}

func (inst preparedInstruction) opcodeText() string {
	return inst.canonical.Op
}

func decodePreparedPayload[T any](inst ir.Instruction) (*T, error) {
	payload := new(T)
	if err := json.Unmarshal(inst.Payload, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func prepareInstruction(inst ir.Instruction) (preparedInstruction, error) {
	op, ok := preparedOpcodes[inst.Op]
	if !ok {
		return preparedInstruction{}, fmt.Errorf("unknown opcode %q", inst.Op)
	}
	out := preparedInstruction{
		op: op, control: preparedControlOpcode(op), canonical: inst, constantIndex: -1, localIndex: -1,
		upvalueIndex: -1,
		globalIndex:  -1, jumpPC: -1, callModuleIndex: -1, callFunctionIndex: -1,
	}
	var err error
	switch op {
	case preparedSelect:
		out.selection, err = decodePreparedPayload[ir.SelectPayload](inst)
	case preparedConst:
		out.constant, err = decodePreparedPayload[ir.ConstPayload](inst)
	case preparedZero, preparedTypeAssert, preparedTypeAssertOK, preparedConvert:
		out.typeOperand, err = decodePreparedPayload[ir.TypePayload](inst)
	case preparedUnary, preparedBinary:
		var payload *ir.OperatorPayload
		payload, err = decodePreparedPayload[ir.OperatorPayload](inst)
		if err == nil {
			var ok bool
			out.operator, ok = parsePreparedOperator(payload.Operator)
			if !ok {
				err = fmt.Errorf("unknown operator %q", payload.Operator)
			}
		}
	case preparedMakeSequence:
		out.makeSequence, err = decodePreparedPayload[ir.MakeSequencePayload](inst)
	case preparedMakeMap:
		out.makeMap, err = decodePreparedPayload[ir.MakeMapPayload](inst)
	case preparedMakeStruct:
		out.makeStruct, err = decodePreparedPayload[ir.MakeStructPayload](inst)
	case preparedMakeSlice:
		out.makeSlice, err = decodePreparedPayload[ir.MakeSlicePayload](inst)
	case preparedMakeWaitable:
		out.makeWaitable, err = decodePreparedPayload[ir.MakeWaitablePayload](inst)
	case preparedAppend:
		out.count, err = decodePreparedPayload[ir.CountPayload](inst)
	case preparedLoadField, preparedStoreField:
		out.field, err = decodePreparedPayload[ir.FieldPayload](inst)
	case preparedLoadExport:
		out.export, err = decodePreparedPayload[ir.ExportPayload](inst)
	case preparedInitModule:
		out.initModule, err = decodePreparedPayload[ir.InitModulePayload](inst)
	case preparedLoadLocal, preparedStoreLocal, preparedMapIterInit, preparedMapIterNext, preparedMapIterClose:
		out.local, err = decodePreparedPayload[ir.LocalPayload](inst)
	case preparedLoadUpvalue, preparedStoreUpvalue:
		out.upvalue, err = decodePreparedPayload[ir.UpvaluePayload](inst)
	case preparedLoadGlobal, preparedStoreGlobal:
		out.global, err = decodePreparedPayload[ir.GlobalPayload](inst)
	case preparedAddressOf:
		out.address, err = decodePreparedPayload[ir.AddressPayload](inst)
		if err == nil && out.address.Kind == "export" {
			out.control = true
		}
	case preparedJump, preparedJumpIf:
		out.jump, err = decodePreparedPayload[ir.JumpPayload](inst)
	case preparedCallDirect, preparedTailCallDirect, preparedCallValue, preparedSpawn:
		out.call, err = decodePreparedPayload[ir.CallPayload](inst)
	case preparedCallInterface:
		out.interfaceCall, err = decodePreparedPayload[ir.CallInterfacePayload](inst)
	case preparedMakeClosure:
		out.closure, err = decodePreparedPayload[ir.ClosurePayload](inst)
	case preparedReturn:
		out.returnValue, err = decodePreparedPayload[ir.ReturnPayload](inst)
	case preparedDeferPush:
		if len(inst.Payload) != 0 {
			out.deferValue, err = decodePreparedPayload[ir.DeferPayload](inst)
		}
	case preparedCallFFI:
		out.callFFI, err = decodePreparedPayload[ir.CallFFIPayload](inst)
	case preparedCallIntrinsic:
		out.callIntrinsic, err = decodePreparedPayload[ir.CallIntrinsicPayload](inst)
	}
	return out, err
}

func preparedControlOpcode(op preparedOpcode) bool {
	switch op {
	case preparedSelect:
		return true
	case preparedLoadExport, preparedInitModule,
		preparedCallDirect, preparedTailCallDirect, preparedCallValue, preparedCallInterface,
		preparedSpawn, preparedWaitableSend, preparedWaitableRecv, preparedWaitableRecvOK,
		preparedCallFFI, preparedDeferPush, preparedRecover,
		preparedPanic, preparedReturn:
		return true
	default:
		return false
	}
}
