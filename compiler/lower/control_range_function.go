package lower

import (
	"fmt"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
)

type rangeFunctionInfo struct {
	IteratorType string
	YieldType    string
	ValueTypes   []string
}

type rangeFunctionReturnSlot struct {
	Local string
	Type  string
}

func (l *lowerer) lowerFunctionRange(stmt ast.Statement, scope *funcScope, iterator ir.Expression, userLabel string) ([]ir.Statement, bool) {
	info, ok := l.rangeFunctionInfo(*stmt.Range, scope)
	if !ok {
		l.add("hirgen.range.func.signature", "range function must have shape func(func() bool), func(func(V) bool), or func(func(K, V) bool)", stmt.Range.Span)
		return nil, false
	}
	if !l.validateFunctionRangeTargets(stmt, info.ValueTypes) {
		return nil, false
	}
	rangeScope := l.childScope(scope)
	returnSlots, returnRefs := l.rangeFunctionReturnSlots(rangeScope)
	keyType, valueType := "", ""
	if len(info.ValueTypes) >= 1 {
		keyType = info.ValueTypes[0]
	}
	if len(info.ValueTypes) == 2 {
		valueType = info.ValueTypes[1]
	}
	if !l.declareRangeShortTargets(stmt, keyType, valueType, rangeScope) {
		return nil, false
	}
	iteratorLocal := l.newSyntheticLocal(rangeScope, "range.func.iter", info.IteratorType)
	keepLocal := l.newSyntheticLocal(rangeScope, "range.func.keep", "Bool")
	returnFlagLocal := l.newSyntheticLocal(rangeScope, "range.func.returning", "Bool")
	iteratorRef := ir.Expression{Kind: ir.ExprLocal, Local: iteratorLocal, Type: l.hirType(info.IteratorType)}
	returnFlagRef := ir.Expression{Kind: ir.ExprLocal, Local: returnFlagLocal, Type: l.hirType("Bool")}
	trueValue := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Bool"), Value: constant.Scalar(`true`)}
	falseValue := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Bool"), Value: constant.Scalar(`false`)}
	yield, ok := l.buildRangeYieldFunction(stmt, info, rangeScope, keepLocal, returnFlagLocal, returnSlots, userLabel)
	if !ok {
		return nil, false
	}
	returnLabel := l.newLabel("range.func.return")
	endLabel := l.newLabel("range.func.end")
	out := []ir.Statement{
		{Kind: ir.StmtStoreLocal, Local: iteratorLocal, Expr: iterator},
		{Kind: ir.StmtStoreLocal, Local: keepLocal, Expr: trueValue},
		{Kind: ir.StmtStoreLocal, Local: returnFlagLocal, Expr: falseValue},
		{Kind: ir.StmtExpr, Expr: ir.Expression{
			Kind:        ir.ExprCallValue,
			Operand:     &iteratorRef,
			Args:        []ir.Expression{yield},
			ResultCount: 0,
		}},
		{Kind: ir.StmtJumpIf, Expr: returnFlagRef, Label: returnLabel},
		{Kind: ir.StmtJump, Label: endLabel},
		{Kind: ir.StmtLabel, Label: returnLabel},
		{Kind: ir.StmtReturn, Results: returnRefs},
		{Kind: ir.StmtLabel, Label: l.newLabel("return.after")},
		{Kind: ir.StmtLabel, Label: endLabel},
	}
	return out, true
}

func (l *lowerer) buildRangeYieldFunction(
	stmt ast.Statement,
	info rangeFunctionInfo,
	rangeScope *funcScope,
	keepLocal string,
	returnFlagLocal string,
	returnSlots []rangeFunctionReturnSlot,
	userLabel string,
) (ir.Expression, bool) {
	l.nextAnonFunc++
	fnIndex := l.nextAnonFunc
	fn := ir.Function{
		ID:            fmt.Sprintf("fn.range.yield.%d", fnIndex),
		Name:          fmt.Sprintf("range.yield.%d", fnIndex),
		RevisionLocal: true,
		Generated:     true,
		Signature:     l.hirSignature(signatureFromTypes(info.ValueTypes, []string{"Bool"}), false),
	}
	yieldScope := newFuncScope(&fn, rangeScope)
	yieldScope.deferOwnerDepth = 2
	paramRefs := make([]ir.Expression, len(info.ValueTypes))
	for i, typ := range info.ValueTypes {
		localID := fmt.Sprintf("local.range.yield.%d.arg.%d", fnIndex, i)
		fn.Locals = append(fn.Locals, ir.Local{
			ID:        localID,
			Name:      fmt.Sprintf("yield.arg.%d", i),
			Type:      l.hirType(typ),
			Scope:     yieldScope.debugScope,
			Generated: true,
		})
		paramRefs[i] = ir.Expression{Kind: ir.ExprLocal, Local: localID, Type: l.hirType(typ)}
	}
	keepName := fmt.Sprintf("range.yield.keep.%d", fnIndex)
	keepUpvalue := "up." + keepName
	fn.Upvalues = append(fn.Upvalues, ir.Upvalue{ID: keepUpvalue, Name: keepName, Type: l.hirType("Bool")})
	yieldScope.captures[keepName] = ir.CaptureTarget{Kind: "local", Local: keepLocal}
	returnFlagName := fmt.Sprintf("range.yield.returning.%d", fnIndex)
	returnFlagUpvalue := "up." + returnFlagName
	fn.Upvalues = append(fn.Upvalues, ir.Upvalue{ID: returnFlagUpvalue, Name: returnFlagName, Type: l.hirType("Bool")})
	yieldScope.captures[returnFlagName] = ir.CaptureTarget{Kind: "local", Local: returnFlagLocal}
	returnTargets := make([]ir.StoreTarget, 0, len(returnSlots))
	for i, slot := range returnSlots {
		upvalueName := fmt.Sprintf("range.yield.result.%d.%d", fnIndex, i)
		upvalueID := "up." + upvalueName
		fn.Upvalues = append(fn.Upvalues, ir.Upvalue{ID: upvalueID, Name: upvalueName, Type: l.hirType(slot.Type)})
		yieldScope.captures[upvalueName] = ir.CaptureTarget{Kind: "local", Local: slot.Local}
		returnTargets = append(returnTargets, ir.StoreTarget{Kind: "upvalue", Upvalue: upvalueID})
	}
	keepRef := ir.Expression{Kind: ir.ExprUpvalue, Upvalue: keepUpvalue, Type: l.hirType("Bool")}
	trueValue := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Bool"), Value: constant.Scalar(`true`)}
	falseValue := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Bool"), Value: constant.Scalar(`false`)}
	activeLabel := l.newLabel("range.yield.active")
	continueLabel := l.newLabel("range.yield.continue")
	stopLabel := l.newLabel("range.yield.stop")
	yieldScope.rangeReturn = &rangeFunctionReturnContext{
		expectedReturns:   rangeScope.expectedReturns,
		resultTypes:       append([]types.TypeRef(nil), rangeScope.resultTypes...),
		namedResultLocals: make(map[string]string, len(rangeScope.namedResultLocals)),
		resultTargets:     returnTargets,
		returnFlagUpvalue: returnFlagUpvalue,
		stopLabel:         stopLabel,
	}
	for name, local := range rangeScope.namedResultLocals {
		yieldScope.rangeReturn.namedResultLocals[name] = local
	}
	body := []ir.Statement{
		{Kind: ir.StmtJumpIf, Expr: keepRef, Label: activeLabel},
		{Kind: ir.StmtReturn, Results: []ir.Expression{falseValue}},
		{Kind: ir.StmtLabel, Label: activeLabel},
	}
	targetStores, ok := l.lowerRangeTargets(stmt, paramRefs, &yieldScope)
	if !ok {
		return ir.Expression{}, false
	}
	body = append(body, targetStores...)
	l.pushBranchWithUserLabel(userLabel, stopLabel, continueLabel)
	loweredBody, ok := l.lowerBlockInScope(stmt.Body, &yieldScope)
	l.popBranch()
	if !ok {
		return ir.Expression{}, false
	}
	body = append(body, loweredBody...)
	body = append(body,
		ir.Statement{Kind: ir.StmtLabel, Label: continueLabel},
		ir.Statement{Kind: ir.StmtReturn, Results: []ir.Expression{trueValue}},
		ir.Statement{Kind: ir.StmtLabel, Label: stopLabel},
		ir.Statement{Kind: ir.StmtStoreUpvalue, Upvalue: keepUpvalue, Expr: falseValue},
		ir.Statement{Kind: ir.StmtReturn, Results: []ir.Expression{falseValue}},
	)
	fn.Body = body
	captures := make([]ir.CaptureTarget, 0, len(fn.Upvalues))
	for _, upvalue := range fn.Upvalues {
		capture, ok := yieldScope.captures[upvalue.Name]
		if !ok {
			l.add("hirgen.range.func.capture", "range yield capture was not recorded", stmt.Span)
			return ir.Expression{}, false
		}
		captures = append(captures, capture)
	}
	l.extraFunctions = append(l.extraFunctions, fn)
	return ir.Expression{Kind: ir.ExprFunction, Function: fn.ID, Type: l.hirType(info.YieldType), Captures: captures}, true
}

func (l *lowerer) rangeFunctionReturnSlots(scope *funcScope) ([]rangeFunctionReturnSlot, []ir.Expression) {
	if scope == nil || scope.expectedReturns == 0 {
		return nil, nil
	}
	if len(scope.namedResults) != 0 {
		slots := make([]rangeFunctionReturnSlot, 0, len(scope.namedResults))
		refs := make([]ir.Expression, 0, len(scope.namedResults))
		for i, result := range scope.namedResults {
			typ := ""
			if i < len(scope.resultTypes) {
				typ = l.typeRefString(scope.resultTypes[i])
			}
			slots = append(slots, rangeFunctionReturnSlot{Local: result.Local, Type: typ})
			refs = append(refs, result)
		}
		return slots, refs
	}
	slots := make([]rangeFunctionReturnSlot, 0, scope.expectedReturns)
	refs := make([]ir.Expression, 0, scope.expectedReturns)
	for i := 0; i < scope.expectedReturns; i++ {
		typ := "Any"
		if i < len(scope.resultTypes) && scope.resultTypes[i].Valid() {
			typ = l.typeRefString(scope.resultTypes[i])
		}
		local := l.newSyntheticLocal(scope, "range.func.result", typ)
		slots = append(slots, rangeFunctionReturnSlot{Local: local, Type: typ})
		refs = append(refs, ir.Expression{Kind: ir.ExprLocal, Local: local, Type: l.hirType(typ)})
	}
	return slots, refs
}

func (l *lowerer) validateFunctionRangeTargets(stmt ast.Statement, valueTypes []string) bool {
	targetCount := 0
	if stmt.Key != nil {
		targetCount++
	}
	if stmt.Value != nil {
		targetCount++
	}
	if targetCount > len(valueTypes) {
		l.add("hirgen.range.func.targets", "range function target count exceeds yielded value count", stmt.Span)
		return false
	}
	if len(valueTypes) == 0 && targetCount != 0 {
		l.add("hirgen.range.func.targets", "zero-value range function does not accept iteration targets", stmt.Span)
		return false
	}
	return true
}

func (l *lowerer) rangeFunctionInfo(expr ast.Expression, scope *funcScope) (rangeFunctionInfo, bool) {
	typ := l.expressionType(expr, scope)
	iteratorParams, iteratorResults, iteratorVariadic, ok := l.functionSignatureParts(typ)
	if !ok || iteratorVariadic || len(iteratorParams) != 1 || len(iteratorResults) != 0 {
		return rangeFunctionInfo{}, false
	}
	yieldParams, yieldResults, yieldVariadic, ok := l.functionSignatureParts(iteratorParams[0])
	if !ok || yieldVariadic || len(yieldParams) > 2 || len(yieldResults) != 1 || l.resolveNamedUnderlyingType(yieldResults[0]) != "Bool" {
		return rangeFunctionInfo{}, false
	}
	return rangeFunctionInfo{
		IteratorType: typ,
		YieldType:    iteratorParams[0],
		ValueTypes:   yieldParams,
	}, true
}
