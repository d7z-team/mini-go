package emit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func lowerExpression(artifact *ir.Artifact, fn *ir.Function, expr hir.Expression) error {
	exprType := expr.Type
	switch expr.Kind {
	case hir.ExprLiteral:
		id := expr.ConstantID
		if id == "" {
			id = fmt.Sprintf("c.%d", len(artifact.Constants))
		}
		artifact.Constants = append(artifact.Constants, ir.Constant{
			ID:      id,
			Type:    exprType,
			Value:   expr.Value,
			Untyped: expr.Untyped,
		})
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpConst),
			Payload: instructionPayload(ir.ConstPayload{Constant: id}),
		})
	case hir.ExprConst:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpConst),
			Payload: instructionPayload(ir.ConstPayload{Constant: expr.ConstantID}),
		})
	case hir.ExprZero:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpZero),
			Payload: instructionPayload(ir.TypePayload{Type: exprType}),
		})
	case hir.ExprLocal:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpLoadLocal),
			Payload: instructionPayload(ir.LocalPayload{Local: expr.Local}),
		})
	case hir.ExprUpvalue:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpLoadUpvalue),
			Payload: instructionPayload(ir.UpvaluePayload{Upvalue: expr.Upvalue}),
		})
	case hir.ExprGlobal:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpLoadGlobal),
			Payload: instructionPayload(ir.GlobalPayload{Global: expr.Global}),
		})
	case hir.ExprLet:
		if strings.TrimSpace(expr.Local) == "" || expr.Bind == nil || expr.Body == nil {
			return errors.New("let expression missing local, bind, or body")
		}
		if err := lowerExpression(artifact, fn, *expr.Bind); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreLocal),
			Payload: instructionPayload(ir.LocalPayload{Local: expr.Local, Rebind: true}),
		})
		if err := lowerExpression(artifact, fn, *expr.Body); err != nil {
			return err
		}
	case hir.ExprLetResults:
		if len(expr.Locals) == 0 || expr.Bind == nil || expr.Body == nil {
			return errors.New("let_results expression missing locals, bind, or body")
		}
		if expressionResultCount(*expr.Bind) != len(expr.Locals) {
			return errors.New("let_results expression result count does not match locals")
		}
		if err := lowerExpression(artifact, fn, *expr.Bind); err != nil {
			return err
		}
		for i := len(expr.Locals) - 1; i >= 0; i-- {
			if strings.TrimSpace(expr.Locals[i]) == "" {
				return errors.New("let_results expression contains empty local")
			}
			fn.Instructions = append(fn.Instructions, ir.Instruction{
				Op:      string(ir.OpStoreLocal),
				Payload: instructionPayload(ir.LocalPayload{Local: expr.Locals[i], Rebind: true}),
			})
		}
		if err := lowerExpression(artifact, fn, *expr.Body); err != nil {
			return err
		}
	case hir.ExprValues:
		for _, value := range expr.Elements {
			if expressionResultCount(value) != 1 {
				return errors.New("values expression element must produce exactly one result")
			}
			if err := lowerExpression(artifact, fn, value); err != nil {
				return err
			}
		}
	case hir.ExprUnary:
		if expr.Operand == nil {
			return errors.New("unary expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpUnary),
			Payload: instructionPayload(ir.OperatorPayload{Operator: expr.Operator}),
		})
	case hir.ExprBinary:
		if expr.Left == nil || expr.Right == nil {
			return errors.New("binary expression missing operand")
		}
		if expr.Operator == "&&" || expr.Operator == "||" {
			return lowerLogicalBinary(artifact, fn, expr)
		}
		if err := lowerExpression(artifact, fn, *expr.Left); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Right); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpBinary),
			Payload: instructionPayload(ir.OperatorPayload{Operator: expr.Operator}),
		})
	case hir.ExprCallDirect:
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpCallDirect),
			Payload: instructionPayload(ir.CallPayload{
				ModulePath:  expr.ModulePath,
				Function:    expr.Function,
				ArgCount:    totalExpressionResults(expr.Args),
				ResultCount: expr.ResultCount,
			}),
		})
	case hir.ExprCallFFI:
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpCallFFI),
			Payload: instructionPayload(ir.CallFFIPayload{
				ArgCount: totalExpressionResults(expr.Args), ResultCount: expr.ResultCount,
			}),
		})
	case hir.ExprCallIntrinsic:
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpCallIntrinsic),
			Payload: instructionPayload(ir.CallIntrinsicPayload{
				ID: ir.IntrinsicID(expr.Intrinsic), ArgCount: totalExpressionResults(expr.Args), ResultCount: expr.ResultCount,
			}),
		})
	case hir.ExprCallValue:
		if expr.Operand == nil {
			return errors.New("call expression missing function operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpCallValue),
			Payload: instructionPayload(ir.CallPayload{
				ArgCount:    totalExpressionResults(expr.Args),
				ResultCount: expr.ResultCount,
			}),
		})
	case hir.ExprCallInterface:
		if expr.Operand == nil {
			return errors.New("interface call expression missing receiver operand")
		}
		if !exprType.Valid() {
			return errors.New("interface call expression missing interface type")
		}
		if strings.TrimSpace(expr.Field) == "" {
			return errors.New("interface call expression missing method")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpCallInterface),
			Payload: instructionPayload(ir.CallInterfacePayload{
				InterfaceType: exprType,
				Method:        expr.Field,
				ArgCount:      totalExpressionResults(expr.Args),
				ResultCount:   expr.ResultCount,
			}),
		})
	case hir.ExprFunction:
		captures, err := lowerCaptures(expr.Captures)
		if err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpMakeClosure),
			Payload: instructionPayload(ir.ClosurePayload{ModulePath: expr.ModulePath, Function: expr.Function, Captures: captures}),
		})
	case hir.ExprLoadExport:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpLoadExport),
			Payload: instructionPayload(ir.ExportPayload{
				ModulePath: expr.ModulePath,
				Export:     expr.Export,
			}),
		})
	case hir.ExprSequence:
		for _, elem := range expr.Elements {
			if err := lowerExpression(artifact, fn, elem); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpMakeSequence),
			Payload: instructionPayload(ir.MakeSequencePayload{Type: exprType, ElementCount: len(expr.Elements)}),
		})
	case hir.ExprMap:
		for _, entry := range expr.Entries {
			if err := lowerExpression(artifact, fn, entry.Key); err != nil {
				return err
			}
			if err := lowerExpression(artifact, fn, entry.Value); err != nil {
				return err
			}
		}
		if expr.Size != nil {
			if err := lowerExpression(artifact, fn, *expr.Size); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpMakeMap),
			Payload: instructionPayload(ir.MakeMapPayload{
				Type:        exprType,
				EntryCount:  len(expr.Entries),
				HasCapacity: expr.Size != nil,
			}),
		})
	case hir.ExprStruct:
		fields := make([]string, 0, len(expr.Fields))
		for _, field := range expr.Fields {
			fields = append(fields, field.Name)
			if err := lowerExpression(artifact, fn, field.Value); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpMakeStruct),
			Payload: instructionPayload(ir.MakeStructPayload{Type: exprType, Fields: fields}),
		})
	case hir.ExprMakeSlice:
		if len(expr.Args) != 1 && len(expr.Args) != 2 {
			return errors.New("make_slice expression requires length and optional capacity")
		}
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpMakeSlice),
			Payload: instructionPayload(ir.MakeSlicePayload{Type: exprType, HasCapacity: len(expr.Args) == 2}),
		})
	case hir.ExprMakeChan:
		if len(expr.Args) != 1 {
			return errors.New("make_chan expression requires capacity")
		}
		if err := lowerExpression(artifact, fn, expr.Args[0]); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpMakeWaitable),
			Payload: instructionPayload(ir.MakeWaitablePayload{Type: exprType}),
		})
	case hir.ExprLoadIndex:
		if expr.Operand == nil || expr.Index == nil {
			return errors.New("index expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Index); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpLoadIndex)})
	case hir.ExprLoadIndexOK:
		if expr.Operand == nil || expr.Index == nil {
			return errors.New("load_index_ok expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Index); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpLoadIndexOK)})
	case hir.ExprStringRuneAt:
		if expr.Operand == nil || expr.Index == nil {
			return errors.New("string_rune_at expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Index); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpStringRuneAt)})
	case hir.ExprStringNextRuneIndex:
		if expr.Operand == nil || expr.Index == nil {
			return errors.New("string_next_rune_index expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Index); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpStringNextRuneIndex)})
	case hir.ExprSlice:
		if expr.Operand == nil || expr.Start == nil || expr.End == nil {
			return errors.New("slice expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Start); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.End); err != nil {
			return err
		}
		maxExpr := hir.Expression{Kind: hir.ExprZero, Type: types.VoidType()}
		if expr.Max != nil {
			maxExpr = *expr.Max
		}
		if err := lowerExpression(artifact, fn, maxExpr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpSlice)})
	case hir.ExprLen:
		if expr.Operand == nil {
			return errors.New("len expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpLen)})
	case hir.ExprCap:
		if expr.Operand == nil {
			return errors.New("cap expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpCap)})
	case hir.ExprAppend:
		if expr.Operand == nil {
			return errors.New("append expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		for _, arg := range expr.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpAppend),
			Payload: instructionPayload(ir.CountPayload{Count: len(expr.Args), Expand: expr.Ellipsis}),
		})
	case hir.ExprDelete:
		if expr.Operand == nil || expr.Index == nil {
			return errors.New("delete expression missing map or key")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Index); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpDelete)})
	case hir.ExprClear:
		if expr.Operand == nil {
			return errors.New("clear expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpClear)})
	case hir.ExprCopy:
		if expr.Left == nil || expr.Right == nil {
			return errors.New("copy expression missing dst or src")
		}
		if err := lowerExpression(artifact, fn, *expr.Left); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Right); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpCopy)})
	case hir.ExprMapIterNext:
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpMapIterNext), Payload: instructionPayload(ir.LocalPayload{Local: expr.Local})})
	case hir.ExprMapKeys:
		if expr.Operand == nil {
			return errors.New("map_keys expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpMapKeys)})
	case hir.ExprLoadField:
		if expr.Operand == nil {
			return errors.New("member expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpLoadField),
			Payload: instructionPayload(ir.FieldPayload{Field: expr.Field}),
		})
	case hir.ExprTypeAssert:
		if expr.Operand == nil {
			return errors.New("type_assert expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpTypeAssert),
			Payload: instructionPayload(ir.TypePayload{Type: exprType}),
		})
	case hir.ExprTypeAssertOK:
		if expr.Operand == nil {
			return errors.New("type_assert_ok expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpTypeAssertOK),
			Payload: instructionPayload(ir.TypePayload{Type: exprType}),
		})
	case hir.ExprConvert:
		if expr.Operand == nil {
			return errors.New("convert expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpConvert),
			Payload: instructionPayload(ir.TypePayload{Type: exprType}),
		})
	case hir.ExprAddressOf:
		payload := ir.AddressPayload{}
		if expr.ModulePath != "" {
			payload.Kind = "export"
			payload.ModulePath = expr.ModulePath
			payload.Export = expr.Export
		} else if expr.Local != "" {
			payload.Kind = "local"
			payload.Local = expr.Local
		} else if expr.Upvalue != "" {
			payload.Kind = "upvalue"
			payload.Upvalue = expr.Upvalue
		} else if expr.Global != "" {
			payload.Kind = "global"
			payload.Global = expr.Global
		} else {
			return errors.New("address expression missing target")
		}
		payload.Path = make([]ir.AddressPathSegment, 0, len(expr.Path))
		for _, segment := range expr.Path {
			payload.Path = append(payload.Path, ir.AddressPathSegment{
				Kind:  segment.Kind,
				Field: segment.Field,
				Local: segment.Local,
			})
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpAddressOf),
			Payload: instructionPayload(payload),
		})
	case hir.ExprLoadIndirect:
		if expr.Operand == nil {
			return errors.New("deref expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpLoadIndirect)})
	case hir.ExprChanRecv:
		if expr.Operand == nil {
			return errors.New("chan_recv expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableRecv)})
	case hir.ExprChanRecvOK:
		if expr.Operand == nil {
			return errors.New("chan_recv_ok expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableRecvOK)})
	case hir.ExprChanCanRecv:
		if expr.Operand == nil {
			return errors.New("chan_ready_recv expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableCanRecv)})
	case hir.ExprChanTryRecv:
		if expr.Operand == nil {
			return errors.New("chan_try_recv expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableTryRecv)})
	case hir.ExprChanTrySend:
		if expr.Operand == nil || expr.Right == nil {
			return errors.New("chan_try_send expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, *expr.Right); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableTrySend)})
	case hir.ExprChanCanSend:
		if expr.Operand == nil {
			return errors.New("chan_ready_send expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableCanSend)})
	case hir.ExprChanClose:
		if expr.Operand == nil {
			return errors.New("chan_close expression missing operand")
		}
		if err := lowerExpression(artifact, fn, *expr.Operand); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableClose)})
	case hir.ExprRecover:
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpRecover)})
	default:
		return fmt.Errorf("unsupported HIR expression kind %q", expr.Kind)
	}
	return nil
}

func lowerLogicalBinary(artifact *ir.Artifact, fn *ir.Function, expr hir.Expression) error {
	boolType := types.Builtin(types.PrimitiveBool)
	slot := addSyntheticLocal(fn, "logical", boolType)
	if err := lowerExpression(artifact, fn, *expr.Left); err != nil {
		return err
	}
	fn.Instructions = append(fn.Instructions, ir.Instruction{
		Op:      string(ir.OpStoreLocal),
		Payload: instructionPayload(ir.LocalPayload{Local: slot}),
	})
	fn.Instructions = append(fn.Instructions, ir.Instruction{
		Op:      string(ir.OpLoadLocal),
		Payload: instructionPayload(ir.LocalPayload{Local: slot}),
	})
	rightLabel := newSyntheticLabel(fn, "logical.right")
	endLabel := newSyntheticLabel(fn, "logical.end")
	switch expr.Operator {
	case "&&":
		fn.Instructions = append(fn.Instructions,
			ir.Instruction{
				Op:      string(ir.OpJumpIf),
				Payload: instructionPayload(ir.JumpPayload{Label: rightLabel}),
			},
			ir.Instruction{
				Op:      string(ir.OpJump),
				Payload: instructionPayload(ir.JumpPayload{Label: endLabel}),
			},
			ir.Instruction{
				Op:      string(ir.OpLabel),
				Payload: instructionPayload(ir.LabelPayload{Label: rightLabel}),
			},
		)
	case "||":
		fn.Instructions = append(fn.Instructions,
			ir.Instruction{
				Op:      string(ir.OpJumpIf),
				Payload: instructionPayload(ir.JumpPayload{Label: endLabel}),
			},
			ir.Instruction{
				Op:      string(ir.OpLabel),
				Payload: instructionPayload(ir.LabelPayload{Label: rightLabel}),
			},
		)
	default:
		return fmt.Errorf("unsupported logical operator %q", expr.Operator)
	}
	if err := lowerExpression(artifact, fn, *expr.Right); err != nil {
		return err
	}
	fn.Instructions = append(fn.Instructions,
		ir.Instruction{
			Op:      string(ir.OpStoreLocal),
			Payload: instructionPayload(ir.LocalPayload{Local: slot}),
		},
		ir.Instruction{
			Op:      string(ir.OpLabel),
			Payload: instructionPayload(ir.LabelPayload{Label: endLabel}),
		},
		ir.Instruction{
			Op:      string(ir.OpLoadLocal),
			Payload: instructionPayload(ir.LocalPayload{Local: slot}),
		},
	)
	return nil
}

func addSyntheticLocal(fn *ir.Function, prefix string, typ types.TypeRef) string {
	id := fmt.Sprintf("__lower.%s.%d", prefix, len(fn.Locals))
	fn.Locals = append(fn.Locals, ir.Local{
		ID:   id,
		Type: typ,
	})
	return id
}

func newSyntheticLabel(fn *ir.Function, prefix string) string {
	return fmt.Sprintf("__lower.%s.%d", prefix, len(fn.Instructions))
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

func instructionPayload(value any) json.RawMessage {
	out, err := ir.MarshalPayload(value)
	if err != nil {
		panic(err.Error())
	}
	return out
}
