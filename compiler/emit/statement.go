package emit

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func (e *packageEmitter) lowerFunction(function *hir.Function, symbols *ir.FunctionSymbols) (ir.SlotCode, error) {
	l := slotLowerer{packageEmitter: e, locals: make(map[string]types.TypeRef, len(function.Locals)), upvalues: make(map[string]types.TypeRef, len(function.Upvalues))}
	for _, local := range function.Locals {
		l.locals[local.ID] = local.Type
	}
	for _, upvalue := range function.Upvalues {
		l.upvalues[upvalue.ID] = upvalue.Type
	}
	for _, statement := range closeExitedMapIterators(function.Body) {
		start := len(l.code.Instructions)
		if err := l.statement(statement); err != nil {
			return ir.SlotCode{}, fmt.Errorf("%s: %w", function.ID, err)
		}
		if symbols != nil && start < len(l.code.Instructions) {
			if len(statement.SourcePoints) != 0 {
				points := make([]ir.Location, len(statement.SourcePoints))
				for i, point := range statement.SourcePoints {
					points[i] = lowerLocation(point)
				}
				symbols.Locations = append(symbols.Locations, ir.InstructionSymbol{PC: start, Points: points})
			}
			addScopeRange(symbols, statement.Scope, start, len(l.code.Instructions))
		}
	}
	return l.code, l.err
}

// branch consumes a predicate directly. Logical subexpressions retain source
// order and short circuit without materializing join values or boolean copies.
func (l *slotLowerer) branch(condition hir.Expression, target string, when bool) error {
	for condition.Kind == hir.ExprUnary && condition.Operator == "!" && condition.Operand != nil {
		condition, when = *condition.Operand, !when
	}
	if condition.Kind == hir.ExprBinary && condition.Left != nil && condition.Right != nil {
		switch condition.Operator {
		case "&&", "||":
			if (condition.Operator == "||") == when {
				if err := l.branch(*condition.Left, target, when); err != nil {
					return err
				}
				return l.branch(*condition.Right, target, when)
			}
			l.labels++
			end := "slot.branch.end." + strconv.Itoa(l.labels)
			if err := l.branch(*condition.Left, end, !when); err != nil {
				return err
			}
			if err := l.branch(*condition.Right, target, when); err != nil {
				return err
			}
			l.emit(ir.OpLabel, ir.LabelPayload{Label: end}, nil)
			return nil
		case "==", "!=", "<", "<=", ">", ">=":
			inputs, err := l.expressions([]hir.Expression{*condition.Left, *condition.Right})
			if err != nil {
				return err
			}
			l.emit(ir.OpCompareBranch, ir.CompareBranchPayload{Operator: condition.Operator, Type: l.operandType(inputs[0]), Label: target, When: when}, inputs)
			return nil
		}
	}
	inputs, err := l.expression(condition)
	if err != nil {
		return err
	}
	l.emit(ir.OpJumpIf, ir.JumpPayload{Label: target, Negate: !when}, inputs)
	return nil
}

func (l *slotLowerer) statement(stmt hir.Statement) error {
	var op ir.Opcode
	var payload ir.Payload
	var children []hir.Expression
	switch stmt.Kind {
	case hir.StmtExpr:
		_, err := l.expression(stmt.Expr)
		return err
	case hir.StmtReturn:
		if len(stmt.Results) == 1 {
			condition := stmt.Results[0]
			if condition.Kind == hir.ExprBinary && (condition.Operator == "&&" || condition.Operator == "||") && condition.Left != nil && condition.Right != nil {
				l.labels++
				short := "slot.return.short." + strconv.Itoa(l.labels)
				when := condition.Operator == "||"
				if err := l.branch(*condition.Left, short, when); err != nil {
					return err
				}
				if err := l.statement(hir.Statement{Kind: hir.StmtReturn, Results: []hir.Expression{*condition.Right}}); err != nil {
					return err
				}
				l.emit(ir.OpLabel, ir.LabelPayload{Label: short}, nil)
				value := []byte("false")
				if when {
					value = []byte("true")
				}
				return l.statement(hir.Statement{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprLiteral, Type: types.Builtin(types.PrimitiveBool), Value: constant.Scalar(string(value))}}})
			}
		}
		op, payload, children = ir.OpReturn, ir.ReturnPayload{ResultCount: totalExpressionResults(stmt.Results)}, stmt.Results
	case hir.StmtTailCallDirect:
		if stmt.Expr.Kind != hir.ExprCallDirect {
			return errors.New("tail call requires direct call")
		}
		op, children = ir.OpTailCallDirect, stmt.Expr.Args
		payload = ir.CallPayload{ModulePath: stmt.Expr.ModulePath, Function: stmt.Expr.Function, ArgCount: totalExpressionResults(children), ResultCount: stmt.Expr.ResultCount}
	case hir.StmtStoreLocal:
		op, payload, children = ir.OpStoreLocal, ir.LocalPayload{Local: stmt.Local, Rebind: stmt.Rebind}, []hir.Expression{stmt.Expr}
	case hir.StmtStoreUpvalue:
		op, payload, children = ir.OpStoreUpvalue, ir.UpvaluePayload{Upvalue: stmt.Upvalue}, []hir.Expression{stmt.Expr}
	case hir.StmtStoreGlobal:
		op, payload, children = ir.OpStoreGlobal, ir.GlobalPayload{Global: stmt.Global}, []hir.Expression{stmt.Expr}
	case hir.StmtStoreResults, hir.StmtStoreValues:
		children = stmt.Values
		if stmt.Kind == hir.StmtStoreResults {
			children = []hir.Expression{stmt.Expr}
		}
		inputs, err := l.expressions(children)
		if err != nil {
			return err
		}
		if len(inputs) != len(stmt.Targets) {
			return errors.New("assignment arity mismatch")
		}
		// Inputs already capture all right-hand values. Commit destinations in
		// source order, including repeated names in a parallel assignment.
		for i, target := range stmt.Targets {
			switch target.Kind {
			case "local":
				l.emit(ir.OpStoreLocal, ir.LocalPayload{Local: target.Local, Rebind: target.Rebind}, inputs[i:i+1])
			case "upvalue":
				l.emit(ir.OpStoreUpvalue, ir.UpvaluePayload{Upvalue: target.Upvalue}, inputs[i:i+1])
			case "global":
				l.emit(ir.OpStoreGlobal, ir.GlobalPayload{Global: target.Global}, inputs[i:i+1])
			case "discard":
			default:
				return fmt.Errorf("unknown assignment target %q", target.Kind)
			}
		}
		return nil
	case hir.StmtStoreIndex:
		op, children = ir.OpStoreIndex, []hir.Expression{stmt.Object, stmt.Index, stmt.Expr}
	case hir.StmtStoreField:
		op, payload, children = ir.OpStoreField, ir.FieldPayload{Field: stmt.Field}, []hir.Expression{stmt.Object, stmt.Expr}
	case hir.StmtStoreIndirect:
		op, children = ir.OpStoreIndirect, []hir.Expression{stmt.Object, stmt.Expr}
	case hir.StmtChanSend:
		op, children = ir.OpWaitableSend, []hir.Expression{stmt.Object, stmt.Expr}
	case hir.StmtSpawn:
		op, payload = ir.OpSpawn, ir.CallPayload{ArgCount: totalExpressionResults(stmt.Args)}
		children = append([]hir.Expression{stmt.Expr}, stmt.Args...)
	case hir.StmtPanic:
		op, children = ir.OpPanic, []hir.Expression{stmt.Expr}
	case hir.StmtDefer:
		op, payload, children = ir.OpDeferPush, ir.DeferPayload{OwnerDepth: stmt.DeferOwnerDepth}, []hir.Expression{stmt.Expr}
	case hir.StmtJumpIf:
		return l.branch(stmt.Expr, stmt.Label, !stmt.BranchNegated)
	case hir.StmtJump:
		op, payload = ir.OpJump, ir.JumpPayload{Label: stmt.Label}
	case hir.StmtLabel:
		op, payload = ir.OpLabel, ir.LabelPayload{Label: stmt.Label}
	case hir.StmtInitModule:
		op, payload = ir.OpInitModule, ir.InitModulePayload{ModulePath: stmt.Module}
	case hir.StmtMapIterInit, hir.StmtMapIterClose:
		op, payload = ir.OpMapIterClose, ir.LocalPayload{Local: stmt.Local}
		if stmt.Kind == hir.StmtMapIterInit {
			op, children = ir.OpMapIterInit, []hir.Expression{stmt.Expr}
		}
	case hir.StmtSelect:
		cases := make([]ir.SelectCase, len(stmt.SelectCases))
		for i, selected := range stmt.SelectCases {
			cases[i] = ir.SelectCase{Channel: selected.Channel, Send: selected.Send, Value: selected.Value, OK: selected.OK}
		}
		op, payload = ir.OpSelect, ir.SelectPayload{Index: stmt.Local, Default: stmt.SelectDefault, Cases: cases}
	case hir.StmtTypeDispatch:
		cases := make([]ir.TypeCase, len(stmt.TypeCases))
		for i, match := range stmt.TypeCases {
			cases[i] = ir.TypeCase{Type: match.Type, Label: match.Label, Binding: match.Binding, Original: match.Original}
		}
		op, payload = ir.OpTypeDispatch, ir.TypeDispatchPayload{Subject: stmt.Local, Default: stmt.Label, DefaultLocal: stmt.DefaultLocal, Cases: cases}
	default:
		return fmt.Errorf("unsupported statement %q", stmt.Kind)
	}
	inputs, err := l.expressions(children)
	if err != nil {
		return err
	}
	l.emit(op, payload, inputs)
	return nil
}

func addScopeRange(symbols *ir.FunctionSymbols, scopeID, start, end int) {
	for scopeID != 0 {
		index := -1
		for i := range symbols.Scopes {
			if symbols.Scopes[i].ID == scopeID {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		ranges := symbols.Scopes[index].Ranges
		if len(ranges) != 0 && ranges[len(ranges)-1].End == start {
			ranges[len(ranges)-1].End = end
			symbols.Scopes[index].Ranges = ranges
		} else {
			symbols.Scopes[index].Ranges = append(symbols.Scopes[index].Ranges, ir.PCRange{Start: start, End: end})
		}
		scopeID = symbols.Scopes[index].Parent
	}
}

func lowerLocation(loc hir.Location) ir.Location {
	return ir.Location{File: loc.File, Line: loc.Line, Column: loc.Column}
}
