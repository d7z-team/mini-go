package emit

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/hir"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func lowerStatement(artifact *ir.Artifact, fn *ir.Function, symbols *ir.FunctionSymbols, stmt hir.Statement) error {
	start := len(fn.Instructions)
	if err := lowerStatementBody(artifact, fn, stmt); err != nil {
		return err
	}
	if len(stmt.SourcePoints) != 0 && start < len(fn.Instructions) {
		index := -1
		for i := range symbols.Locations {
			if symbols.Locations[i].PC == start {
				index = i
				break
			}
		}
		if index < 0 {
			symbols.Locations = append(symbols.Locations, ir.InstructionSymbol{PC: start})
			index = len(symbols.Locations) - 1
		}
		seen := make(map[ir.Location]struct{}, len(stmt.SourcePoints)+len(symbols.Locations[index].Points))
		for _, loc := range symbols.Locations[index].Points {
			seen[loc] = struct{}{}
		}
		for _, loc := range stmt.SourcePoints {
			lowered := lowerLocation(loc)
			if _, exists := seen[lowered]; exists {
				continue
			}
			symbols.Locations[index].Points = append(symbols.Locations[index].Points, lowered)
			seen[lowered] = struct{}{}
		}
	}
	if start < len(fn.Instructions) && stmt.Scope != 0 {
		addScopeRange(symbols, stmt.Scope, start, len(fn.Instructions))
	}
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

func lowerStatementBody(artifact *ir.Artifact, fn *ir.Function, stmt hir.Statement) error {
	switch stmt.Kind {
	case hir.StmtSelect:
		payload := ir.SelectPayload{Index: stmt.Local, Default: stmt.SelectDefault}
		for _, selected := range stmt.SelectCases {
			payload.Cases = append(payload.Cases, ir.SelectCase{Channel: selected.Channel, Send: selected.Send, Value: selected.Value, OK: selected.OK})
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpSelect), Payload: instructionPayload(payload)})
		return nil
	case hir.StmtMapIterInit, hir.StmtMapIterClose:
		op := ir.OpMapIterClose
		if stmt.Kind == hir.StmtMapIterInit {
			if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
				return err
			}
			op = ir.OpMapIterInit
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(op), Payload: instructionPayload(ir.LocalPayload{Local: stmt.Local})})
	case hir.StmtExpr:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		for i := 0; i < expressionResultCount(stmt.Expr); i++ {
			fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpPop)})
		}
	case hir.StmtReturn:
		count := 0
		for _, expr := range stmt.Results {
			if err := lowerExpression(artifact, fn, expr); err != nil {
				return err
			}
			count += expressionResultCount(expr)
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpReturn),
			Payload: instructionPayload(ir.ReturnPayload{ResultCount: count}),
		})
	case hir.StmtTailCallDirect:
		call := stmt.Expr
		if call.Kind != hir.ExprCallDirect {
			return errors.New("tail_call_direct requires a direct call expression")
		}
		for _, arg := range call.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpTailCallDirect),
			Payload: instructionPayload(ir.CallPayload{
				ModulePath: call.ModulePath, Function: call.Function,
				ArgCount: totalExpressionResults(call.Args), ResultCount: call.ResultCount,
			}),
		})
	case hir.StmtStoreLocal:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreLocal),
			Payload: instructionPayload(ir.LocalPayload{Local: stmt.Local, Rebind: stmt.Rebind}),
		})
	case hir.StmtStoreUpvalue:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreUpvalue),
			Payload: instructionPayload(ir.UpvaluePayload{Upvalue: stmt.Upvalue}),
		})
	case hir.StmtStoreGlobal:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreGlobal),
			Payload: instructionPayload(ir.GlobalPayload{Global: stmt.Global}),
		})
	case hir.StmtStoreResults:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		count := expressionResultCount(stmt.Expr)
		if count != len(stmt.Targets) {
			return fmt.Errorf("store_results target count mismatch: expression has %d results, got %d targets", count, len(stmt.Targets))
		}
		for i := len(stmt.Targets) - 1; i >= 0; i-- {
			if err := lowerStoreTarget(fn, stmt.Targets[i]); err != nil {
				return err
			}
		}
	case hir.StmtStoreValues:
		count := 0
		for _, value := range stmt.Values {
			if err := lowerExpression(artifact, fn, value); err != nil {
				return err
			}
			count += expressionResultCount(value)
		}
		if count != len(stmt.Targets) {
			return fmt.Errorf("store_values target count mismatch: values have %d results, got %d targets", count, len(stmt.Targets))
		}
		for i := len(stmt.Targets) - 1; i >= 0; i-- {
			if err := lowerStoreTarget(fn, stmt.Targets[i]); err != nil {
				return err
			}
		}
	case hir.StmtStoreIndex:
		if err := lowerExpression(artifact, fn, stmt.Object); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, stmt.Index); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpStoreIndex)})
	case hir.StmtStoreField:
		if err := lowerExpression(artifact, fn, stmt.Object); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreField),
			Payload: instructionPayload(ir.FieldPayload{Field: stmt.Field}),
		})
	case hir.StmtStoreIndirect:
		if err := lowerExpression(artifact, fn, stmt.Object); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpStoreIndirect)})
	case hir.StmtChanSend:
		if err := lowerExpression(artifact, fn, stmt.Object); err != nil {
			return err
		}
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpWaitableSend)})
	case hir.StmtSpawn:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		for _, arg := range stmt.Args {
			if err := lowerExpression(artifact, fn, arg); err != nil {
				return err
			}
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op: string(ir.OpSpawn),
			Payload: instructionPayload(ir.CallPayload{
				ArgCount:    totalExpressionResults(stmt.Args),
				ResultCount: 0,
			}),
		})
	case hir.StmtInitModule:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpInitModule),
			Payload: instructionPayload(ir.InitModulePayload{ModulePath: stmt.Module}),
		})
	case hir.StmtPanic:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpPanic)})
	case hir.StmtDefer:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		inst := ir.Instruction{Op: string(ir.OpDeferPush)}
		if stmt.DeferOwnerDepth > 0 {
			inst.Payload = instructionPayload(ir.DeferPayload{OwnerDepth: stmt.DeferOwnerDepth})
		}
		fn.Instructions = append(fn.Instructions, inst)
	case hir.StmtLabel:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpLabel),
			Payload: instructionPayload(ir.LabelPayload{Label: stmt.Label}),
		})
	case hir.StmtJump:
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpJump),
			Payload: instructionPayload(ir.JumpPayload{Label: stmt.Label}),
		})
	case hir.StmtJumpIf:
		if err := lowerExpression(artifact, fn, stmt.Expr); err != nil {
			return err
		}
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpJumpIf),
			Payload: instructionPayload(ir.JumpPayload{Label: stmt.Label}),
		})
	default:
		return fmt.Errorf("unsupported HIR statement kind %q", stmt.Kind)
	}
	return nil
}

func lowerLocation(loc hir.Location) ir.Location {
	return ir.Location{File: loc.File, Line: loc.Line, Column: loc.Column}
}

func lowerStoreTarget(fn *ir.Function, target hir.StoreTarget) error {
	switch target.Kind {
	case "local":
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreLocal),
			Payload: instructionPayload(ir.LocalPayload{Local: target.Local, Rebind: target.Rebind}),
		})
	case "upvalue":
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreUpvalue),
			Payload: instructionPayload(ir.UpvaluePayload{Upvalue: target.Upvalue}),
		})
	case "global":
		fn.Instructions = append(fn.Instructions, ir.Instruction{
			Op:      string(ir.OpStoreGlobal),
			Payload: instructionPayload(ir.GlobalPayload{Global: target.Global}),
		})
	case "discard":
		fn.Instructions = append(fn.Instructions, ir.Instruction{Op: string(ir.OpPop)})
	default:
		return fmt.Errorf("unsupported store_results target kind %q", target.Kind)
	}
	return nil
}
