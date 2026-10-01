package emit

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// slotLowerer preserves values explicitly while descending the HIR. No
// serialized operand-stack program is involved in producing this graph.
type slotLowerer struct {
	*packageEmitter
	locals   map[string]types.TypeRef
	upvalues map[string]types.TypeRef
	code     ir.SlotCode
	labels   int
	err      error
}

type packageEmitter struct {
	artifact   *ir.Artifact
	functions  map[string]types.FunctionSignature
	constants  map[string]uint32
	globals    map[string]types.TypeRef
	containers map[slotContainerType]types.TypeRef
}

type slotContainerType struct {
	kind    types.Kind
	element types.TypeRef
}

func newPackageEmitter(artifact *ir.Artifact, functions []hir.Function) *packageEmitter {
	e := &packageEmitter{artifact: artifact, functions: make(map[string]types.FunctionSignature, len(functions)), constants: make(map[string]uint32, len(artifact.Constants)), globals: make(map[string]types.TypeRef, len(artifact.Globals))}
	for _, function := range functions {
		e.functions[function.ID] = function.Signature
	}
	for index, constant := range artifact.Constants {
		e.constants[constant.ID] = uint32(index)
	}
	for _, global := range artifact.Globals {
		e.globals[global.ID] = global.Type
	}
	e.containers = make(map[slotContainerType]types.TypeRef)
	for _, node := range artifact.TypeTable.Nodes {
		if node.Kind == types.Pointer || node.Kind == types.Slice {
			key := slotContainerType{kind: node.Kind, element: node.Elem}
			if _, found := e.containers[key]; !found {
				e.containers[key] = types.Ref(node)
			}
		}
	}
	return e
}

func (l *slotLowerer) emit(op ir.Opcode, payload ir.Payload, inputs []ir.Operand, results ...types.TypeRef) []ir.Operand {
	operands := ir.SlotOperands{Inputs: inputs}
	outputs := make([]ir.Operand, len(results))
	for i, typ := range results {
		index := uint32(len(l.code.Types))
		l.code.Types = append(l.code.Types, typ)
		operands.Outputs = append(operands.Outputs, index)
		outputs[i] = ir.Operand{Kind: ir.OperandSlot, Index: index}
	}
	l.operation(op, payload, operands)
	return outputs
}

func (l *slotLowerer) operation(op ir.Opcode, payload ir.Payload, operands ir.SlotOperands) {
	if l.err != nil {
		return
	}
	index, err := l.code.Descriptors.Append(payload)
	if err != nil {
		l.err = err
		return
	}
	l.code.Instructions = append(l.code.Instructions, ir.SlotInstruction{Op: op, Descriptor: index, Operands: uint32(len(l.code.Operands))})
	l.code.Operands = append(l.code.Operands, operands)
}

func (l *slotLowerer) operandType(operand ir.Operand) types.TypeRef {
	if operand.Kind == ir.OperandConstant {
		return l.artifact.Constants[operand.Index].Type
	}
	return l.code.Types[operand.Index]
}

func (l *slotLowerer) expressions(expressions []hir.Expression) ([]ir.Operand, error) {
	var operands []ir.Operand
	for _, expression := range expressions {
		values, err := l.expression(expression)
		if err != nil {
			return nil, err
		}
		operands = append(operands, values...)
	}
	return operands, nil
}

func (l *slotLowerer) expression(expr hir.Expression) ([]ir.Operand, error) {
	var op ir.Opcode
	var payload ir.Payload
	var children []hir.Expression
	var results []types.TypeRef
	if expr.Type.Valid() {
		results = []types.TypeRef{expr.Type}
	}
	switch expr.Kind {
	case hir.ExprLoadField:
		var names []string
		root := expr
		for root.Kind == hir.ExprLoadField && root.Operand != nil && len(names) < 16 {
			names = append(names, root.Field)
			root = *root.Operand
		}
		if len(names) == 0 {
			return nil, errors.New("field access missing operand")
		}
		inputs, err := l.expression(root)
		if err != nil || len(inputs) != 1 {
			return nil, fmt.Errorf("field path root: %v", err)
		}
		typ := l.operandType(inputs[0])
		path := ir.FieldPathPayload{Type: typ}
		for i := len(names) - 1; i >= 0; i-- {
			view := types.View(&l.artifact.TypeTable, typ)
			if view.Shape() == types.Pointer {
				element, _ := view.Elem()
				view = types.View(&l.artifact.TypeTable, element)
			}
			fields, _ := view.StructFields()
			found := false
			for index, field := range fields {
				if field.Name == names[i] && field.Name != "_" {
					path.Fields = append(path.Fields, uint32(index))
					typ, found = field.Type, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("field %q not found in path", names[i])
			}
		}
		return l.emit(ir.OpGetPath, path, inputs, typ), nil
	case hir.ExprLiteral:
		index := len(l.artifact.Constants)
		id := expr.ConstantID
		if id == "" {
			id = "c." + strconv.Itoa(index)
		}
		l.artifact.Constants = append(l.artifact.Constants, ir.Constant{ID: id, Type: expr.Type, Value: expr.Value, Untyped: expr.Untyped})
		l.constants[id] = uint32(index)
		return []ir.Operand{{Kind: ir.OperandConstant, Index: uint32(index)}}, nil
	case hir.ExprConst:
		if index, found := l.constants[expr.ConstantID]; found {
			return []ir.Operand{{Kind: ir.OperandConstant, Index: index}}, nil
		}
		return nil, fmt.Errorf("constant %q not found", expr.ConstantID)
	case hir.ExprZero:
		op, payload = ir.OpZero, ir.TypePayload{Type: expr.Type}
	case hir.ExprLocal, hir.ExprUpvalue, hir.ExprGlobal:
		switch expr.Kind {
		case hir.ExprLocal:
			op, payload = ir.OpLoadLocal, ir.LocalPayload{Local: expr.Local}
			results = []types.TypeRef{l.locals[expr.Local]}
		case hir.ExprUpvalue:
			op, payload = ir.OpLoadUpvalue, ir.UpvaluePayload{Upvalue: expr.Upvalue}
			results = []types.TypeRef{l.upvalues[expr.Upvalue]}
		case hir.ExprGlobal:
			op, payload = ir.OpLoadGlobal, ir.GlobalPayload{Global: expr.Global}
			results = []types.TypeRef{l.globals[expr.Global]}
		}
	case hir.ExprValues:
		return l.expressions(expr.Elements)
	case hir.ExprLet, hir.ExprLetResults:
		if expr.Bind == nil || expr.Body == nil {
			return nil, fmt.Errorf("%s missing binding or body", expr.Kind)
		}
		values, err := l.expression(*expr.Bind)
		if err != nil {
			return nil, err
		}
		locals := expr.Locals
		if expr.Kind == hir.ExprLet {
			locals = []string{expr.Local}
		}
		if len(values) != len(locals) {
			return nil, fmt.Errorf("%s binding arity mismatch", expr.Kind)
		}
		for i := len(locals) - 1; i >= 0; i-- {
			l.emit(ir.OpStoreLocal, ir.LocalPayload{Local: locals[i], Rebind: true}, values[i:i+1])
		}
		return l.expression(*expr.Body)
	case hir.ExprUnary:
		if expr.Operand == nil {
			return nil, errors.New("unary missing operand")
		}
		op, payload, children = ir.OpUnary, ir.OperatorPayload{Operator: expr.Operator}, []hir.Expression{*expr.Operand}
	case hir.ExprBinary:
		if expr.Left == nil || expr.Right == nil {
			return nil, errors.New("binary missing operand")
		}
		if expr.Operator == "&&" || expr.Operator == "||" {
			return l.logical(expr)
		}
		op, payload, children = ir.OpBinary, ir.OperatorPayload{Operator: expr.Operator}, []hir.Expression{*expr.Left, *expr.Right}
	case hir.ExprCallDirect, hir.ExprCallValue, hir.ExprCallInterface, hir.ExprCallFFI, hir.ExprCallIntrinsic:
		if expr.Kind == hir.ExprCallValue || expr.Kind == hir.ExprCallInterface {
			if expr.Operand == nil {
				return nil, fmt.Errorf("%s missing callee", expr.Kind)
			}
			children = append(children, *expr.Operand)
		}
		children = append(children, expr.Args...)
		results = expr.ResultTypes
		if len(results) != expr.ResultCount && expr.Kind == hir.ExprCallDirect && (expr.ModulePath == "" || expr.ModulePath == l.artifact.Module.Path) {
			results = l.functions[expr.Function].Results
		}
		if len(results) != expr.ResultCount {
			return nil, fmt.Errorf("%s missing result types: have %d, want %d", expr.Kind, len(results), expr.ResultCount)
		}
		count := totalExpressionResults(expr.Args)
		switch expr.Kind {
		case hir.ExprCallDirect, hir.ExprCallValue:
			op = ir.OpCallDirect
			if expr.Kind == hir.ExprCallValue {
				op = ir.OpCallValue
			}
			payload = ir.CallPayload{ModulePath: expr.ModulePath, Function: expr.Function, ArgCount: count, ResultCount: len(results)}
		case hir.ExprCallInterface:
			op, payload = ir.OpCallInterface, ir.CallInterfacePayload{InterfaceType: expr.Type, Method: expr.Field, ArgCount: count, ResultCount: len(results)}
		case hir.ExprCallFFI:
			op, payload = ir.OpCallFFI, ir.CallFFIPayload{ArgCount: count, ResultCount: len(results)}
		case hir.ExprCallIntrinsic:
			op, payload = ir.OpCallIntrinsic, ir.CallIntrinsicPayload{ID: ir.IntrinsicID(expr.Intrinsic), ArgCount: count, ResultCount: len(results)}
		}
	case hir.ExprStruct:
		fields := make([]string, len(expr.Fields))
		for i, field := range expr.Fields {
			fields[i] = field.Name
			children = append(children, field.Value)
		}
		op, payload = ir.OpMakeStruct, ir.MakeStructPayload{Type: expr.Type, Fields: fields}
	case hir.ExprSequence:
		children = expr.Elements
		op, payload = ir.OpMakeSequence, ir.MakeSequencePayload{Type: expr.Type, ElementCount: len(children)}
	case hir.ExprMap:
		for _, entry := range expr.Entries {
			children = append(children, entry.Key, entry.Value)
		}
		if expr.Size != nil {
			children = append(children, *expr.Size)
		}
		op, payload = ir.OpMakeMap, ir.MakeMapPayload{Type: expr.Type, EntryCount: len(expr.Entries), HasCapacity: expr.Size != nil}
	case hir.ExprFunction:
		captures, err := lowerCaptures(expr.Captures)
		if err != nil {
			return nil, err
		}
		op, payload = ir.OpMakeClosure, ir.ClosurePayload{ModulePath: expr.ModulePath, Function: expr.Function, Captures: captures}
		results = expr.ResultTypes
		if len(results) == 0 && (expr.ModulePath == "" || expr.ModulePath == l.artifact.Module.Path) {
			signature, found := l.functions[expr.Function]
			if !found {
				return nil, fmt.Errorf("function %q not found", expr.Function)
			}
			typ, err := l.structuralType(types.TypeNode{Kind: types.Function, Signature: &signature})
			if err != nil {
				return nil, err
			}
			results = []types.TypeRef{typ}
		}
	case hir.ExprLoadExport:
		op, payload = ir.OpLoadExport, ir.ExportPayload{ModulePath: expr.ModulePath, Export: expr.Export}
	case hir.ExprMakeSlice, hir.ExprMakeChan:
		children = expr.Args
		if expr.Kind == hir.ExprMakeSlice {
			if len(children) != 1 && len(children) != 2 {
				return nil, errors.New("make slice requires length and optional capacity")
			}
			op, payload = ir.OpMakeSlice, ir.MakeSlicePayload{Type: expr.Type, HasCapacity: len(children) == 2}
		} else {
			if len(children) != 1 {
				return nil, errors.New("make channel requires capacity")
			}
			op, payload = ir.OpMakeWaitable, ir.MakeWaitablePayload{Type: expr.Type}
		}
	case hir.ExprLoadIndex, hir.ExprLoadIndexOK, hir.ExprStringRuneAt, hir.ExprStringNextRuneIndex, hir.ExprDelete:
		if expr.Operand == nil || expr.Index == nil {
			return nil, fmt.Errorf("%s missing collection or index", expr.Kind)
		}
		children = []hir.Expression{*expr.Operand, *expr.Index}
		switch expr.Kind {
		case hir.ExprLoadIndex:
			op = ir.OpLoadIndex
		case hir.ExprLoadIndexOK:
			op = ir.OpLoadIndexOK
		case hir.ExprStringRuneAt:
			op = ir.OpStringRuneAt
		case hir.ExprStringNextRuneIndex:
			op = ir.OpStringNextRuneIndex
		case hir.ExprDelete:
			op, results = ir.OpDelete, nil
		}
	case hir.ExprSlice:
		if expr.Operand == nil || expr.Start == nil || expr.End == nil {
			return nil, errors.New("slice missing operand or bounds")
		}
		children = []hir.Expression{*expr.Operand, *expr.Start, *expr.End}
		if expr.Max == nil {
			children = append(children, hir.Expression{Kind: hir.ExprZero, Type: types.VoidType()})
		} else {
			children = append(children, *expr.Max)
		}
		op = ir.OpSlice
	case hir.ExprCopy:
		if expr.Left == nil || expr.Right == nil {
			return nil, errors.New("copy missing destination or source")
		}
		op, children = ir.OpCopy, []hir.Expression{*expr.Left, *expr.Right}
		results = []types.TypeRef{types.Builtin(types.PrimitiveInt)}
	case hir.ExprAppend:
		if expr.Operand == nil {
			return nil, errors.New("append missing slice")
		}
		children = append([]hir.Expression{*expr.Operand}, expr.Args...)
		op, payload = ir.OpAppend, ir.CountPayload{Count: len(expr.Args), Expand: expr.Ellipsis}
	case hir.ExprMapIterNext:
		op, payload, results = ir.OpMapIterNext, ir.LocalPayload{Local: expr.Local}, expr.ResultTypes
		if len(results) == 0 {
			key, value, found := types.View(&l.artifact.TypeTable, l.locals[expr.Local]).Map()
			if found {
				results = []types.TypeRef{key, value, types.Builtin(types.PrimitiveBool)}
			}
		}
	case hir.ExprAddressOf:
		address := ir.AddressPayload{}
		var base types.TypeRef
		switch {
		case expr.ModulePath != "":
			address.Kind, address.ModulePath, address.Export = "export", expr.ModulePath, expr.Export
		case expr.Local != "":
			address.Kind, address.Local = "local", expr.Local
			base = l.locals[expr.Local]
		case expr.Upvalue != "":
			address.Kind, address.Upvalue = "upvalue", expr.Upvalue
			base = l.upvalues[expr.Upvalue]
		case expr.Global != "":
			address.Kind, address.Global = "global", expr.Global
			base = l.globals[expr.Global]
		default:
			return nil, errors.New("address missing target")
		}
		for _, segment := range expr.Path {
			address.Path = append(address.Path, ir.AddressPathSegment{Kind: segment.Kind, Field: segment.Field, Local: segment.Local})
			if !base.Valid() {
				continue
			}
			view := types.View(&l.artifact.TypeTable, base)
			switch segment.Kind {
			case "index", "indirect":
				base, _ = view.Elem()
			case "field":
				if view.Shape() == types.Pointer {
					element, _ := view.Elem()
					view = types.View(&l.artifact.TypeTable, element)
				}
				fields, _ := view.StructFields()
				base = types.TypeRef{}
				for _, field := range fields {
					if field.Name == segment.Field {
						base = field.Type
						break
					}
				}
			default:
				return nil, fmt.Errorf("unknown address path %q", segment.Kind)
			}
		}
		if base.Valid() && len(results) == 0 && len(expr.ResultTypes) == 0 {
			typ, err := l.structuralType(types.TypeNode{Kind: types.Pointer, Elem: base})
			if err != nil {
				return nil, err
			}
			results = []types.TypeRef{typ}
		}
		op, payload = ir.OpAddressOf, address
	case hir.ExprRecover:
		op, results = ir.OpRecover, []types.TypeRef{types.AnyType()}
	case hir.ExprLen, hir.ExprCap, hir.ExprClear, hir.ExprMapKeys,
		hir.ExprTypeAssert, hir.ExprTypeAssertOK, hir.ExprConvert, hir.ExprLoadIndirect,
		hir.ExprChanRecv, hir.ExprChanRecvOK, hir.ExprChanCanRecv, hir.ExprChanTryRecv,
		hir.ExprChanTrySend, hir.ExprChanCanSend, hir.ExprChanClose:
		if expr.Operand == nil {
			return nil, fmt.Errorf("%s missing operand", expr.Kind)
		}
		children = []hir.Expression{*expr.Operand}
		switch expr.Kind {
		case hir.ExprLen:
			op = ir.OpLen
		case hir.ExprCap:
			op = ir.OpCap
		case hir.ExprClear:
			op, results = ir.OpClear, nil
		case hir.ExprMapKeys:
			op = ir.OpMapKeys
		case hir.ExprTypeAssert:
			op, payload = ir.OpTypeAssert, ir.TypePayload{Type: expr.Type}
		case hir.ExprTypeAssertOK:
			op, payload = ir.OpTypeAssertOK, ir.TypePayload{Type: expr.Type}
			results = []types.TypeRef{expr.Type, types.Builtin(types.PrimitiveBool)}
		case hir.ExprConvert:
			if expr.Type == expr.Operand.Type && !expr.Operand.Untyped && ir.IsDirectLocalType(&l.artifact.TypeTable, expr.Type) {
				return l.expression(*expr.Operand)
			}
			op, payload = ir.OpConvert, ir.TypePayload{Type: expr.Type}
		case hir.ExprLoadIndirect:
			op = ir.OpLoadIndirect
		case hir.ExprChanRecv:
			op = ir.OpWaitableRecv
		case hir.ExprChanRecvOK:
			op = ir.OpWaitableRecvOK
		case hir.ExprChanCanRecv:
			op = ir.OpWaitableCanRecv
		case hir.ExprChanTryRecv:
			op = ir.OpWaitableTryRecv
		case hir.ExprChanTrySend:
			if expr.Right == nil {
				return nil, errors.New("channel send missing value")
			}
			children = append(children, *expr.Right)
			op = ir.OpWaitableTrySend
		case hir.ExprChanCanSend:
			op = ir.OpWaitableCanSend
		case hir.ExprChanClose:
			op, results = ir.OpWaitableClose, nil
		}
	default:
		return nil, fmt.Errorf("slot lowering for %q is not defined", expr.Kind)
	}
	inputs, err := l.expressions(children)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 && expressionResultCount(expr) != 0 {
		results = expr.ResultTypes
		if len(results) == 0 && len(inputs) != 0 {
			view := types.View(&l.artifact.TypeTable, l.operandType(inputs[0]))
			switch expr.Kind {
			case hir.ExprLen, hir.ExprCap, hir.ExprStringNextRuneIndex:
				results = []types.TypeRef{types.Builtin(types.PrimitiveInt)}
			case hir.ExprStringRuneAt:
				results = []types.TypeRef{types.Builtin(types.PrimitiveInt32)}
			case hir.ExprChanCanRecv, hir.ExprChanCanSend, hir.ExprChanTrySend:
				results = []types.TypeRef{types.Builtin(types.PrimitiveBool)}
			case hir.ExprLoadIndirect, hir.ExprChanRecv, hir.ExprChanRecvOK, hir.ExprChanTryRecv, hir.ExprLoadIndex, hir.ExprLoadIndexOK:
				typ, found := view.Elem()
				if !found {
					_, typ, found = view.Map()
				}
				if !found && view.Shape() == types.Primitive {
					if primitive, _ := view.Primitive(); primitive == types.PrimitiveString {
						typ, found = types.Builtin(types.PrimitiveUint8), true
					}
				}
				if found {
					results = []types.TypeRef{typ}
					if expressionResultCount(expr) == 2 {
						results = append(results, types.Builtin(types.PrimitiveBool))
					}
				}
			case hir.ExprSlice:
				if view.Shape() == types.Pointer {
					element, _ := view.Elem()
					view = types.View(&l.artifact.TypeTable, element)
				}
				if view.Shape() == types.Array {
					_, element, _ := view.Array()
					typ, err := l.structuralType(types.TypeNode{Kind: types.Slice, Elem: element})
					if err != nil {
						return nil, err
					}
					results = []types.TypeRef{typ}
				} else {
					results = []types.TypeRef{l.operandType(inputs[0])}
				}
			case hir.ExprAppend:
				results = []types.TypeRef{l.operandType(inputs[0])}
			}
		}
		if len(results) == 0 && len(inputs) != 0 && (expr.Kind == hir.ExprUnary || expr.Kind == hir.ExprBinary) {
			typ := l.operandType(inputs[0])
			switch expr.Operator {
			case "!", "==", "!=", "<", "<=", ">", ">=":
				typ = types.Builtin(types.PrimitiveBool)
			}
			results = []types.TypeRef{typ}
		}
	}
	if len(results) != expressionResultCount(expr) {
		return nil, fmt.Errorf("%s result arity/type mismatch", expr.Kind)
	}
	return l.emit(op, payload, inputs, results...), nil
}

func (l *slotLowerer) structuralType(node types.TypeNode) (types.TypeRef, error) {
	container := node.Kind == types.Pointer || node.Kind == types.Slice
	key := slotContainerType{kind: node.Kind, element: node.Elem}
	if container {
		if ref, found := l.containers[key]; found {
			return ref, nil
		}
	}
	for index := len(l.artifact.TypeTable.Nodes); ; index++ {
		node.ID = types.TypeID("slot.type." + strconv.Itoa(index))
		ref := types.Ref(node)
		if _, exists := l.artifact.TypeTable.Node(ref); exists {
			continue
		}
		if err := l.artifact.TypeTable.Add(node); err != nil {
			return types.TypeRef{}, err
		}
		if container {
			l.containers[key] = ref
		}
		return ref, nil
	}
}

func (l *slotLowerer) logical(expr hir.Expression) ([]ir.Operand, error) {
	// A branch join writes one explicit destination from either predecessor.
	// Both arms share its type; allocation may reuse it only after the join.
	l.labels++
	rightLabel := "slot.logical.right." + strconv.Itoa(l.labels)
	endLabel := "slot.logical.end." + strconv.Itoa(l.labels)
	left, err := l.expression(*expr.Left)
	if err != nil || len(left) != 1 {
		return nil, fmt.Errorf("logical left operand: %v", err)
	}
	output := []ir.Operand{{Kind: ir.OperandSlot, Index: uint32(len(l.code.Types))}}
	l.code.Types = append(l.code.Types, types.Builtin(types.PrimitiveBool))
	condition := left
	if expr.Operator == "||" {
		condition = l.emit(ir.OpUnary, ir.OperatorPayload{Operator: "!"}, left, types.Builtin(types.PrimitiveBool))
	}
	l.emit(ir.OpJumpIf, ir.JumpPayload{Label: rightLabel}, condition)
	copyInto := func(value ir.Operand) {
		l.operation(ir.OpConvert, ir.TypePayload{Type: types.Builtin(types.PrimitiveBool)}, ir.SlotOperands{Inputs: []ir.Operand{value}, Outputs: []uint32{output[0].Index}})
	}
	copyInto(left[0])
	l.emit(ir.OpJump, ir.JumpPayload{Label: endLabel}, nil)
	l.emit(ir.OpLabel, ir.LabelPayload{Label: rightLabel}, nil)
	right, err := l.expression(*expr.Right)
	if err != nil || len(right) != 1 {
		return nil, fmt.Errorf("logical right operand: %v", err)
	}
	copyInto(right[0])
	l.emit(ir.OpLabel, ir.LabelPayload{Label: endLabel}, nil)
	return output, nil
}

func lowerCaptures(captures []hir.CaptureTarget) ([]ir.AddressPayload, error) {
	out := make([]ir.AddressPayload, 0, len(captures))
	for _, capture := range captures {
		payload := ir.AddressPayload{Kind: capture.Kind}
		switch capture.Kind {
		case "local":
			payload.Local = capture.Local
		case "upvalue":
			payload.Upvalue = capture.Upvalue
		case "global":
			payload.Global = capture.Global
		default:
			return nil, fmt.Errorf("unsupported capture kind %q", capture.Kind)
		}
		out = append(out, payload)
	}
	return out, nil
}

func expressionResultCount(expr hir.Expression) int {
	switch expr.Kind {
	case hir.ExprMapIterNext:
		return 3
	case hir.ExprCallDirect, hir.ExprCallFFI, hir.ExprCallIntrinsic, hir.ExprCallValue, hir.ExprCallInterface:
		return expr.ResultCount
	case hir.ExprChanRecvOK, hir.ExprChanTryRecv:
		return 2
	case hir.ExprLoadIndexOK:
		return 2
	case hir.ExprTypeAssertOK:
		return 2
	case hir.ExprLet, hir.ExprLetResults:
		if expr.Body == nil {
			return 0
		}
		return expressionResultCount(*expr.Body)
	case hir.ExprValues:
		return totalExpressionResults(expr.Elements)
	case hir.ExprDelete, hir.ExprClear, hir.ExprChanClose:
		return 0
	default:
		return 1
	}
}

func totalExpressionResults(expressions []hir.Expression) int {
	count := 0
	for _, expr := range expressions {
		count += expressionResultCount(expr)
	}
	return count
}
