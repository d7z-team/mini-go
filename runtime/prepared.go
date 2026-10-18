package runtime

import (
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type preparedOpcode uint8

const (
	preparedConst preparedOpcode = iota
	preparedZero
	preparedPop
	preparedUnary
	preparedBinary
	preparedMinMax
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
	preparedTypeDispatch
	preparedCompareBranch
	preparedGetPath
	preparedIndexConvert
	preparedLenCompareBranch
	preparedUpdateCompareBranch
)

var preparedOpcodes = map[ir.Opcode]preparedOpcode{
	ir.OpGetPath:             preparedGetPath,
	ir.OpCompareBranch:       preparedCompareBranch,
	ir.OpIndexConvert:        preparedIndexConvert,
	ir.OpLenCompareBranch:    preparedLenCompareBranch,
	ir.OpUpdateCompareBranch: preparedUpdateCompareBranch,
	ir.OpTypeDispatch:        preparedTypeDispatch,
	ir.OpSelect:              preparedSelect,
	ir.OpMapIterInit:         preparedMapIterInit,
	ir.OpMapIterNext:         preparedMapIterNext,
	ir.OpMapIterClose:        preparedMapIterClose,
	ir.OpConst:               preparedConst,
	ir.OpZero:                preparedZero,
	ir.OpPop:                 preparedPop,
	ir.OpUnary:               preparedUnary,
	ir.OpBinary:              preparedBinary,
	ir.OpMinMax:              preparedMinMax,
	ir.OpLoadLocal:           preparedLoadLocal,
	ir.OpStoreLocal:          preparedStoreLocal,
	ir.OpLoadUpvalue:         preparedLoadUpvalue,
	ir.OpStoreUpvalue:        preparedStoreUpvalue,
	ir.OpLoadGlobal:          preparedLoadGlobal,
	ir.OpStoreGlobal:         preparedStoreGlobal,
	ir.OpJump:                preparedJump,
	ir.OpJumpIf:              preparedJumpIf,
	ir.OpReturn:              preparedReturn,
	ir.OpPanic:               preparedPanic,
	ir.OpRecover:             preparedRecover,
	ir.OpDeferPush:           preparedDeferPush,
	ir.OpCallValue:           preparedCallValue,
	ir.OpCallDirect:          preparedCallDirect,
	ir.OpCallInterface:       preparedCallInterface,
	ir.OpTailCallDirect:      preparedTailCallDirect,
	ir.OpMakeClosure:         preparedMakeClosure,
	ir.OpMakeSequence:        preparedMakeSequence,
	ir.OpMakeMap:             preparedMakeMap,
	ir.OpMakeStruct:          preparedMakeStruct,
	ir.OpMakeSlice:           preparedMakeSlice,
	ir.OpMakeWaitable:        preparedMakeWaitable,
	ir.OpLoadIndex:           preparedLoadIndex,
	ir.OpLoadIndexOK:         preparedLoadIndexOK,
	ir.OpStringRuneAt:        preparedStringRuneAt,
	ir.OpStringNextRuneIndex: preparedStringNextRuneIndex,
	ir.OpSlice:               preparedSlice,
	ir.OpLen:                 preparedLen,
	ir.OpCap:                 preparedCap,
	ir.OpAppend:              preparedAppend,
	ir.OpDelete:              preparedDelete,
	ir.OpClear:               preparedClear,
	ir.OpCopy:                preparedCopy,
	ir.OpMapKeys:             preparedMapKeys,
	ir.OpLoadField:           preparedLoadField,
	ir.OpStoreIndex:          preparedStoreIndex,
	ir.OpStoreField:          preparedStoreField,
	ir.OpTypeAssert:          preparedTypeAssert,
	ir.OpTypeAssertOK:        preparedTypeAssertOK,
	ir.OpConvert:             preparedConvert,
	ir.OpAddressOf:           preparedAddressOf,
	ir.OpLoadIndirect:        preparedLoadIndirect,
	ir.OpStoreIndirect:       preparedStoreIndirect,
	ir.OpWaitableSend:        preparedWaitableSend,
	ir.OpWaitableRecv:        preparedWaitableRecv,
	ir.OpWaitableRecvOK:      preparedWaitableRecvOK,
	ir.OpWaitableCanRecv:     preparedWaitableCanRecv,
	ir.OpWaitableTryRecv:     preparedWaitableTryRecv,
	ir.OpWaitableTrySend:     preparedWaitableTrySend,
	ir.OpWaitableCanSend:     preparedWaitableCanSend,
	ir.OpWaitableClose:       preparedWaitableClose,
	ir.OpInitModule:          preparedInitModule,
	ir.OpLoadExport:          preparedLoadExport,
	ir.OpSpawn:               preparedSpawn,
	ir.OpCallFFI:             preparedCallFFI,
	ir.OpCallIntrinsic:       preparedCallIntrinsic,
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
	operands    *ir.RegisterFlow
	op          preparedOpcode
	mode        uint16
	logicalCost uint32
	control     bool
	// fastScalar is loader-proven: the instruction only touches private typed
	// lanes, cannot allocate or block, and has no dynamic fault path. A task may
	// execute consecutive fastScalar instructions without re-entering the owner
	// checks between each dispatch.
	fastScalar bool
	canonical  ir.Instruction

	constant         *ir.ConstPayload
	typeOperand      *ir.TypePayload
	operator         preparedOperator
	numericInputs    [3]types.PrimitiveKind
	makeSequence     *ir.MakeSequencePayload
	makeMap          *ir.MakeMapPayload
	makeStruct       *ir.MakeStructPayload
	makeSlice        *ir.MakeSlicePayload
	makeWaitable     *ir.MakeWaitablePayload
	count            *ir.CountPayload
	field            *ir.FieldPayload
	export           *ir.ExportPayload
	initModule       *ir.InitModulePayload
	local            *ir.LocalPayload
	upvalue          *ir.UpvaluePayload
	global           *ir.GlobalPayload
	address          *ir.AddressPayload
	jump             *ir.JumpPayload
	call             *ir.CallPayload
	interfaceCall    *ir.CallInterfacePayload
	closure          *ir.ClosurePayload
	returnValue      *ir.ReturnPayload
	deferValue       *ir.DeferPayload
	callFFI          *ir.CallFFIPayload
	callIntrinsic    *ir.CallIntrinsicPayload
	selection        *ir.SelectPayload
	typeDispatch     *ir.TypeDispatchPayload
	dispatch         *preparedTypeSwitch
	comparison       *ir.CompareBranchPayload
	updateComparison *ir.UpdateCompareBranchPayload
	updateOperator   preparedOperator
	minMax           *ir.MinMaxPayload
	fieldPath        *ir.FieldPathPayload
	path             []preparedField

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
	conversion        conversionMetadata
	minMaxKind        minMaxValueKind
}

func (inst *preparedInstruction) classifyFastScalar(localTypes []vmType, localEscapes []bool) bool {
	if inst == nil || inst.control || inst.logicalCost == 0 {
		return false
	}
	inputPrimitive := types.PrimitiveKind(inst.mode & 0xff)
	outputPrimitive := types.PrimitiveKind(inst.mode >> 8)
	scalar := func(primitive types.PrimitiveKind) bool {
		return primitive >= types.PrimitiveBool && primitive <= types.PrimitiveComplex128 && primitive != types.PrimitiveString
	}
	switch inst.op {
	case preparedZero:
		return scalar(outputPrimitive)
	case preparedUnary:
		return scalar(inputPrimitive) && scalar(outputPrimitive)
	case preparedBinary:
		if !scalar(inputPrimitive) || !scalar(outputPrimitive) {
			return false
		}
		switch inst.operator {
		case operatorDiv, operatorMod, operatorShiftLeft, operatorShiftRight:
			return false
		default:
			return true
		}
	case preparedCompareBranch:
		return scalar(inputPrimitive)
	case preparedUpdateCompareBranch:
		if !scalar(inputPrimitive) || len(inst.operands.Inputs) != 3 || inst.operands.Inputs[0].Kind != ir.OperandLocal {
			return false
		}
		localIndex := int(inst.operands.Inputs[0].Index)
		return localIndex >= 0 && localIndex < len(localTypes) && localIndex < len(localEscapes) &&
			!localEscapes[localIndex] && directFrameLocal(localTypes[localIndex])
	case preparedLoadLocal, preparedStoreLocal:
		return inst.localIndex >= 0 && inst.localIndex < len(localTypes) &&
			inst.localIndex < len(localEscapes) && !localEscapes[inst.localIndex] &&
			directFrameLocal(localTypes[inst.localIndex])
	case preparedJump:
		return true
	case preparedJumpIf:
		return inputPrimitive == types.PrimitiveBool
	default:
		return false
	}
}

func (inst preparedInstruction) opcodeText() string {
	return inst.canonical.Op.String()
}

func decodePreparedPayload[T any](inst ir.Instruction) (*T, error) {
	if payload, ok := inst.Payload.(T); ok {
		return &payload, nil
	}
	var payload T
	return nil, fmt.Errorf("instruction payload %T does not match %T", inst.Payload, payload)
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
	case preparedTypeDispatch:
		out.typeDispatch, err = decodePreparedPayload[ir.TypeDispatchPayload](inst)
	case preparedConst:
		out.constant, err = decodePreparedPayload[ir.ConstPayload](inst)
	case preparedZero, preparedTypeAssert, preparedTypeAssertOK, preparedConvert, preparedIndexConvert:
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
	case preparedMinMax:
		out.minMax, err = decodePreparedPayload[ir.MinMaxPayload](inst)
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
	case preparedGetPath:
		out.fieldPath, err = decodePreparedPayload[ir.FieldPathPayload](inst)
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
	case preparedCompareBranch, preparedLenCompareBranch:
		out.comparison, err = decodePreparedPayload[ir.CompareBranchPayload](inst)
		if err == nil {
			var ok bool
			out.operator, ok = parsePreparedOperator(out.comparison.Operator)
			if !ok {
				err = fmt.Errorf("unknown comparison %q", out.comparison.Operator)
			}
		}
	case preparedUpdateCompareBranch:
		out.updateComparison, err = decodePreparedPayload[ir.UpdateCompareBranchPayload](inst)
		if err == nil {
			var ok bool
			out.updateOperator, ok = parsePreparedOperator(out.updateComparison.Update)
			if !ok {
				err = fmt.Errorf("unknown update operator %q", out.updateComparison.Update)
			} else if out.operator, ok = parsePreparedOperator(out.updateComparison.Comparison); !ok {
				err = fmt.Errorf("unknown comparison %q", out.updateComparison.Comparison)
			}
		}
	case preparedCallDirect, preparedTailCallDirect, preparedCallValue, preparedSpawn:
		out.call, err = decodePreparedPayload[ir.CallPayload](inst)
	case preparedCallInterface:
		out.interfaceCall, err = decodePreparedPayload[ir.CallInterfacePayload](inst)
	case preparedMakeClosure:
		out.closure, err = decodePreparedPayload[ir.ClosurePayload](inst)
	case preparedReturn:
		out.returnValue, err = decodePreparedPayload[ir.ReturnPayload](inst)
	case preparedDeferPush:
		out.deferValue, err = decodePreparedPayload[ir.DeferPayload](inst)
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
