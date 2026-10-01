package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) lowerMethodCall(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	selector := *expr.Callee
	if selector.Operand == nil || strings.TrimSpace(selector.Field) == "" {
		return ir.Expression{}, false
	}
	selection, selected := l.semanticSelection(selector)
	if !selected || selection.Kind != check.SelectionMethod {
		return ir.Expression{}, false
	}
	if selection.Interface {
		interfaceMethod, ok := l.semanticInterfaceMethodInfo(selector)
		if !ok {
			return ir.Expression{}, false
		}
		receiver, ok := l.lowerExpression(*selector.Operand, scope)
		if !ok {
			return ir.Expression{}, false
		}
		args, ok := l.lowerCallArguments(expr, scope, l.signatureParamTypes(interfaceMethod.Signature), interfaceMethod.Signature.Variadic, nil)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{
			Kind:        ir.ExprCallInterface,
			Type:        interfaceMethod.InterfaceType,
			Field:       interfaceMethod.Method,
			Operand:     &receiver,
			Args:        args,
			ResultCount: len(interfaceMethod.Signature.Results), ResultTypes: interfaceMethod.Signature.Results,
		}, true
	}
	method, _, ok := l.semanticMethodInfo(selector)
	if !ok {
		return ir.Expression{}, false
	}
	path, receiverType, ok := l.semanticSelectionPath(selection)
	if !ok {
		return ir.Expression{}, false
	}
	var receiver ir.Expression
	if selection.Indirect {
		receiver, ok = l.lowerAddressableReceiver(selectorPathExpression(*selector.Operand, path), scope)
	} else {
		receiver, ok = l.lowerExpression(*selector.Operand, scope)
		receiver = projectSelectorPath(receiver, path)
	}
	if !ok {
		return ir.Expression{}, false
	}
	receiver = l.autoDerefMethodReceiver(receiver, receiverType, method)
	args, ok := l.lowerCallArguments(expr, scope, l.signatureParamTypes(method.Signature), method.Signature.Variadic, []ir.Expression{receiver})
	if !ok {
		return ir.Expression{}, false
	}
	return ir.Expression{
		Kind:        ir.ExprCallDirect,
		ModulePath:  method.ModulePath,
		Function:    method.FunctionID,
		Args:        args,
		ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
	}, true
}

func (l *lowerer) lowerOperatorCall(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	operator, ok := l.semanticOperator(expr.NodeID)
	if !ok {
		return ir.Expression{}, false
	}
	selection := operator.Selection
	receiverSource := expr.Left
	var argument *ast.Expression
	if expr.Kind == ast.ExprUnary {
		receiverSource = expr.Operand
	} else {
		argument = expr.Right
	}
	if receiverSource == nil {
		return ir.Expression{}, false
	}
	path, receiverType, ok := l.semanticSelectionPath(selection)
	if !ok {
		return ir.Expression{}, false
	}
	if selection.Interface {
		method, found := l.semanticInterfaceMethodFromSelection(selection)
		if !found {
			return ir.Expression{}, false
		}
		receiver, lowered := l.lowerExpression(*receiverSource, scope)
		if !lowered {
			return ir.Expression{}, false
		}
		receiver = projectSelectorPath(receiver, path)
		args, lowered := l.lowerOperatorArguments(argument, l.signatureParamTypes(method.Signature), scope, nil)
		if !lowered {
			return ir.Expression{}, false
		}
		return ir.Expression{
			Kind: ir.ExprCallInterface, Type: method.InterfaceType, Field: method.Method,
			Operand: &receiver, Args: args, ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
		}, true
	}
	method, found := l.methodInfoFromSelection(selection)
	if !found {
		return ir.Expression{}, false
	}
	var receiver ir.Expression
	if selection.Indirect {
		receiver, ok = l.lowerAddressableReceiver(selectorPathExpression(*receiverSource, path), scope)
	} else {
		receiver, ok = l.lowerExpression(*receiverSource, scope)
		receiver = projectSelectorPath(receiver, path)
	}
	if !ok {
		return ir.Expression{}, false
	}
	receiver = l.autoDerefMethodReceiver(receiver, receiverType, method)
	args, ok := l.lowerOperatorArguments(argument, l.signatureParamTypes(method.Signature), scope, []ir.Expression{receiver})
	if !ok {
		return ir.Expression{}, false
	}
	return ir.Expression{
		Kind: ir.ExprCallDirect, ModulePath: method.ModulePath, Function: method.FunctionID,
		Args: args, ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
	}, true
}

func (l *lowerer) semanticInterfaceMethodFromSelection(selection check.Selection) (interfaceMethodInfo, bool) {
	if !selection.Receiver.Valid() {
		return interfaceMethodInfo{}, false
	}
	return interfaceMethodInfo{
		InterfaceType: selection.Receiver, Method: selection.Name, Signature: selection.Signature,
	}, true
}

func (l *lowerer) lowerOperatorArguments(argument *ast.Expression, params []string, scope *funcScope, prefix []ir.Expression) ([]ir.Expression, bool) {
	args := append([]ir.Expression(nil), prefix...)
	if argument == nil {
		return args, len(params) == 0
	}
	if len(params) != 1 {
		return nil, false
	}
	value, ok := l.lowerExpressionInType(*argument, params[0], scope)
	if !ok {
		return nil, false
	}
	return append(args, value), true
}

func selectorPathExpression(base ast.Expression, path []string) ast.Expression {
	for _, field := range path {
		operand := base
		base = ast.Expression{Kind: ast.ExprSelector, Operand: &operand, Field: field, Span: base.Span}
	}
	return base
}

func projectSelectorPath(receiver ir.Expression, path []string) ir.Expression {
	for _, field := range path {
		operand := receiver
		receiver = ir.Expression{Kind: ir.ExprLoadField, Operand: &operand, Field: field}
	}
	return receiver
}

func (l *lowerer) autoDerefMethodReceiver(receiver ir.Expression, receiverType string, method methodInfo) ir.Expression {
	_, base := l.splitPointerType(receiverType)
	if base == strings.TrimSpace(receiverType) {
		return receiver
	}
	if l.typeRefString(method.Receiver) != base {
		return receiver
	}
	return ir.Expression{Kind: ir.ExprLoadIndirect, Operand: &receiver}
}

func (l *lowerer) lowerAddressableReceiver(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	target, ok := l.lowerAddressTarget(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	return wrapAddressLets(target.expr, target.lets), true
}

func wrapAddressLets(expr ir.Expression, lets []addressLet) ir.Expression {
	for i := len(lets) - 1; i >= 0; i-- {
		bind := lets[i].value
		body := expr
		expr = ir.Expression{Kind: ir.ExprLet, Local: lets[i].local, Bind: &bind, Body: &body}
	}
	return expr
}

func (l *lowerer) splitPointerType(typ string) (bool, string) {
	typ = strings.TrimSpace(typ)
	if elem, ok := l.pointerElementType(typ); ok {
		return true, elem
	}
	return false, typ
}
