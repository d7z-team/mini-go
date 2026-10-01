package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) lowerDefer(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	if stmt.Expr == nil {
		l.add("hirgen.defer.expr.missing", "defer statement requires an expression", stmt.Span)
		return nil, false
	}
	expr := *stmt.Expr
	if expr.Kind != ast.ExprCall {
		l.add("hirgen.defer.call", "defer statement requires a call expression", expr.Span)
		return nil, false
	}
	if expr.Callee == nil {
		l.add("hirgen.defer.callee.missing", "defer call requires a callee", expr.Span)
		return nil, false
	}
	return l.lowerDeferredCall(expr, scope)
}

func (l *lowerer) lowerDeferredCall(expr ast.Expression, scope *funcScope) ([]ir.Statement, bool) {
	out, deferred, ok := l.lowerCapturedCall(expr, scope, "defer")
	if !ok {
		return nil, false
	}
	return append(out, ir.Statement{Kind: ir.StmtDefer, Expr: deferred, DeferOwnerDepth: deferOwnerDepth(scope)}), true
}

func (l *lowerer) lowerCapturedCall(expr ast.Expression, scope *funcScope, prefix string) ([]ir.Statement, ir.Expression, bool) {
	if expr.Callee == nil {
		l.add("hirgen.call.callee.missing", "captured call requires a callee", expr.Span)
		return nil, ir.Expression{}, false
	}
	var out []ir.Statement
	tempArgs := make([]ir.Expression, 0, len(expr.Args))
	captures := make([]ir.CaptureTarget, 0, len(expr.Args)+1)
	upvalues := make([]ir.Upvalue, 0, len(expr.Args)+1)
	paramTypes, variadic, haveParamTypes := l.callParamTypes(*expr.Callee, scope)
	argumentValues := []ir.Expression{}
	argumentsPacked := false
	if haveParamTypes {
		plan, ok := l.planCallArguments(expr, scope, paramTypes, variadic)
		if !ok {
			return nil, ir.Expression{}, false
		}
		out = append(out, plan.prelude()...)
		argumentValues = plan.values
		argumentsPacked = true
	} else {
		for i, arg := range expr.Args {
			targetType := ""
			if builtinTarget, ok := l.capturedBuiltinArgTargetType(expr, i, scope); ok {
				targetType = builtinTarget
			}
			value, ok := l.lowerExpressionInType(arg, targetType, scope)
			if !ok {
				return nil, ir.Expression{}, false
			}
			argumentValues = append(argumentValues, value)
		}
	}
	for i, value := range argumentValues {
		typ := strings.TrimSpace(l.typeRefString(value.Type))
		if typ == "" && i < len(paramTypes) {
			typ = paramTypes[i]
		}
		if typ == "" && !haveParamTypes && i < len(expr.Args) {
			typ = l.expressionType(expr.Args[i], scope)
		}
		if typ == "" {
			typ = "Any"
		}
		local := l.newSyntheticLocal(scope, prefix+".arg", typ)
		out = append(out, ir.Statement{Kind: ir.StmtStoreLocal, Local: local, Expr: value})
		upvalueID := fmt.Sprintf("up.%s.arg.%d", prefix, i)
		upvalues = append(upvalues, ir.Upvalue{ID: upvalueID, Name: fmt.Sprintf("%s.arg.%d", prefix, i), Type: l.hirType(typ)})
		captures = append(captures, ir.CaptureTarget{Kind: "local", Local: local})
		tempArgs = append(tempArgs, ir.Expression{Kind: ir.ExprUpvalue, Upvalue: upvalueID})
	}
	bodyCall, extraOut, extraUpvalues, extraCaptures, ok := l.capturedWrapperCallExpression(expr, tempArgs, scope, argumentsPacked)
	if !ok {
		return nil, ir.Expression{}, false
	}
	out = append(extraOut, out...)
	upvalues = append(extraUpvalues, upvalues...)
	captures = append(extraCaptures, captures...)
	body := ir.Statement{Kind: ir.StmtExpr, Expr: bodyCall}
	if expr.Callee.Kind == ast.ExprIdent && strings.TrimSpace(expr.Callee.Name) == "panic" && l.isBuiltinCallName(expr.Callee.Name, expr.Callee.Span, scope) {
		body.Kind = ir.StmtPanic
	}
	l.nextAnonFunc++
	fn := ir.Function{
		ID:            fmt.Sprintf("fn.%s.%d", prefix, l.nextAnonFunc),
		Name:          fmt.Sprintf("%s.%d", prefix, l.nextAnonFunc),
		RevisionLocal: true,
		Generated:     true,
		Signature:     l.hirSignature("function() Void", false),
		Upvalues:      upvalues,
		Body:          []ir.Statement{body},
	}
	l.extraFunctions = append(l.extraFunctions, fn)
	return out, ir.Expression{Kind: ir.ExprFunction, Function: fn.ID, Captures: captures}, true
}

func deferOwnerDepth(scope *funcScope) int {
	if scope == nil || scope.deferOwnerDepth < 0 {
		return 0
	}
	return scope.deferOwnerDepth
}

func (l *lowerer) capturedBuiltinArgTargetType(expr ast.Expression, index int, scope *funcScope) (string, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent || !l.isBuiltinCallName(expr.Callee.Name, expr.Callee.Span, scope) {
		return "", false
	}
	switch strings.TrimSpace(expr.Callee.Name) {
	case "print", "println":
		return "Any", true
	case "delete":
		if index == 1 && len(expr.Args) >= 1 {
			targetType := l.mapIndexKeyType(expr.Args[0], scope)
			return targetType, targetType != ""
		}
	}
	return "", false
}

func (l *lowerer) capturedWrapperCallExpression(expr ast.Expression, args []ir.Expression, scope *funcScope, argumentsPacked bool) (ir.Expression, []ir.Statement, []ir.Upvalue, []ir.CaptureTarget, bool) {
	callee := *expr.Callee
	if callee.Kind == ast.ExprSelector {
		if call, out, upvalues, captures, ok := l.capturedWrapperMethodCallExpression(expr, args, scope, argumentsPacked); ok {
			return call, out, upvalues, captures, true
		}
	}
	if callee.Kind == ast.ExprIdent && l.isBuiltinCallName(callee.Name, callee.Span, scope) {
		call, ok := l.capturedWrapperBuiltinCallExpression(expr, args, scope)
		return call, nil, nil, nil, ok
	}
	signature, found := l.semanticCallSignature(expr)
	if !found {
		l.add("hirgen.semantic.call", "captured function call is missing semantic signature", expr.Span)
		return ir.Expression{}, nil, nil, nil, false
	}
	fnValue, ok := l.lowerExpression(callee, scope)
	if !ok {
		return ir.Expression{}, nil, nil, nil, false
	}
	if callee.Kind == ast.ExprIdent && fnValue.Kind == ir.ExprFunction {
		callArgs := args
		if !argumentsPacked {
			callArgs, ok = l.packCallArguments(expr, args, l.signatureParamTypes(signature), signature.Variadic, nil)
			if !ok {
				return ir.Expression{}, nil, nil, nil, false
			}
		}
		return ir.Expression{
			Kind: ir.ExprCallDirect, Function: fnValue.Function, Args: callArgs,
			ResultCount: len(signature.Results), ResultTypes: signature.Results,
		}, nil, nil, nil, true
	}
	if expr.Ellipsis && !argumentsPacked {
		l.add("hirgen.call.ellipsis", "ellipsis call requires a known Mini-Go variadic function", expr.Span)
		return ir.Expression{}, nil, nil, nil, false
	}
	functionType := l.expressionType(callee, scope)
	if functionType == "" {
		functionType = "Function"
	}
	local := l.newSyntheticLocal(scope, "defer.fn", functionType)
	store := ir.Statement{Kind: ir.StmtStoreLocal, Local: local, Expr: fnValue}
	upvalue := ir.Upvalue{ID: "up.defer.fn", Name: "defer.fn", Type: l.hirType(functionType)}
	capture := ir.CaptureTarget{Kind: "local", Local: local}
	fnRef := ir.Expression{Kind: ir.ExprUpvalue, Upvalue: upvalue.ID}
	return ir.Expression{
		Kind:        ir.ExprCallValue,
		Operand:     &fnRef,
		Args:        args,
		ResultCount: len(signature.Results), ResultTypes: signature.Results,
	}, []ir.Statement{store}, []ir.Upvalue{upvalue}, []ir.CaptureTarget{capture}, true
}

func (l *lowerer) capturedWrapperBuiltinCallExpression(expr ast.Expression, args []ir.Expression, scope *funcScope) (ir.Expression, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
		l.add("hirgen.builtin.callee", "builtin call requires identifier callee", expr.Span)
		return ir.Expression{}, false
	}
	if expr.Ellipsis {
		l.add("hirgen.call.ellipsis", "ellipsis call requires a known Mini-Go variadic function", expr.Span)
		return ir.Expression{}, false
	}
	name := strings.TrimSpace(expr.Callee.Name)
	switch name {
	case "delete":
		if _, ok := l.validateDeleteBuiltin(expr, scope); !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprDelete, Operand: &args[0], Index: &args[1]}, true
	case "clear":
		if !l.validateClearBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprClear, Operand: &args[0]}, true
	case "close":
		if !l.validateCloseBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprChanClose, Operand: &args[0]}, true
	case "copy":
		if !l.validateCopyBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprCopy, Left: &args[0], Right: &args[1]}, true
	case "panic":
		if len(expr.Args) != 1 || len(args) != 1 {
			l.add("hirgen.builtin.arg_count", "panic builtin requires exactly one argument", expr.Span)
			return ir.Expression{}, false
		}
		return args[0], true
	case "recover":
		if len(expr.Args) != 0 || len(args) != 0 {
			l.add("hirgen.builtin.arg_count", "recover builtin requires no arguments", expr.Span)
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprRecover}, true
	case "print", "println":
		return l.lowerPrintBuiltinCall(name, args), true
	default:
		l.add("hirgen.builtin.defer", "builtin call cannot be deferred", expr.Span)
		return ir.Expression{}, false
	}
}

func (l *lowerer) capturedWrapperMethodCallExpression(expr ast.Expression, args []ir.Expression, scope *funcScope, argumentsPacked bool) (ir.Expression, []ir.Statement, []ir.Upvalue, []ir.CaptureTarget, bool) {
	if expr.Callee == nil {
		return ir.Expression{}, nil, nil, nil, false
	}
	selector := *expr.Callee
	if selector.Operand == nil || strings.TrimSpace(selector.Field) == "" {
		return ir.Expression{}, nil, nil, nil, false
	}
	selection, selected := l.semanticSelection(selector)
	if !selected || selection.Kind != check.SelectionMethod {
		return ir.Expression{}, nil, nil, nil, false
	}
	if selection.Interface {
		interfaceMethod, ok := l.semanticInterfaceMethodInfo(selector)
		if !ok {
			return ir.Expression{}, nil, nil, nil, false
		}
		receiver, ok := l.lowerExpression(*selector.Operand, scope)
		if !ok {
			return ir.Expression{}, nil, nil, nil, false
		}
		local := l.newSyntheticLocal(scope, "defer.recv", l.typeRefString(interfaceMethod.InterfaceType))
		store := ir.Statement{Kind: ir.StmtStoreLocal, Local: local, Expr: receiver}
		upvalue := ir.Upvalue{ID: "up.defer.recv", Name: "defer.recv", Type: interfaceMethod.InterfaceType}
		capture := ir.CaptureTarget{Kind: "local", Local: local}
		receiverRef := ir.Expression{Kind: ir.ExprUpvalue, Upvalue: upvalue.ID}
		callArgs := args
		if !argumentsPacked {
			var ok bool
			callArgs, ok = l.packCallArguments(expr, args, l.signatureParamTypes(interfaceMethod.Signature), interfaceMethod.Signature.Variadic, nil)
			if !ok {
				return ir.Expression{}, nil, nil, nil, false
			}
		}
		return ir.Expression{
			Kind:        ir.ExprCallInterface,
			Type:        interfaceMethod.InterfaceType,
			Field:       interfaceMethod.Method,
			Operand:     &receiverRef,
			Args:        callArgs,
			ResultCount: len(interfaceMethod.Signature.Results), ResultTypes: interfaceMethod.Signature.Results,
		}, []ir.Statement{store}, []ir.Upvalue{upvalue}, []ir.CaptureTarget{capture}, true
	}
	method, _, ok := l.semanticMethodInfo(selector)
	if !ok {
		return ir.Expression{}, nil, nil, nil, false
	}
	path, receiverType, ok := l.semanticSelectionPath(selection)
	if !ok {
		return ir.Expression{}, nil, nil, nil, false
	}
	var receiver ir.Expression
	if selection.Indirect {
		receiver, ok = l.lowerAddressableReceiver(selectorPathExpression(*selector.Operand, path), scope)
	} else {
		receiver, ok = l.lowerExpression(*selector.Operand, scope)
		receiver = projectSelectorPath(receiver, path)
	}
	if !ok {
		return ir.Expression{}, nil, nil, nil, false
	}
	receiver = l.autoDerefMethodReceiver(receiver, receiverType, method)
	local := l.newSyntheticLocal(scope, "defer.recv", l.typeRefString(method.Receiver))
	store := ir.Statement{Kind: ir.StmtStoreLocal, Local: local, Expr: receiver}
	upvalue := ir.Upvalue{ID: "up.defer.recv", Name: "defer.recv", Type: method.Receiver}
	capture := ir.CaptureTarget{Kind: "local", Local: local}
	receiverRef := ir.Expression{Kind: ir.ExprUpvalue, Upvalue: upvalue.ID}
	callArgs := args
	if argumentsPacked {
		callArgs = append([]ir.Expression{receiverRef}, callArgs...)
	} else {
		var ok bool
		callArgs, ok = l.packCallArguments(expr, args, l.signatureParamTypes(method.Signature), method.Signature.Variadic, []ir.Expression{receiverRef})
		if !ok {
			return ir.Expression{}, nil, nil, nil, false
		}
	}
	return ir.Expression{
		Kind:        ir.ExprCallDirect,
		ModulePath:  method.ModulePath,
		Function:    method.FunctionID,
		Args:        callArgs,
		ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
	}, []ir.Statement{store}, []ir.Upvalue{upvalue}, []ir.CaptureTarget{capture}, true
}
