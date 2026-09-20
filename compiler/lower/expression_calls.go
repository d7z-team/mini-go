package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) lowerReceiveExpression(expr ast.Expression, scope *funcScope, twoValue bool) (ir.Expression, bool) {
	operand, ok := l.lowerOperand(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	if twoValue {
		return ir.Expression{Kind: ir.ExprChanRecvOK, Operand: &operand}, true
	}
	return ir.Expression{Kind: ir.ExprChanRecv, Operand: &operand}, true
}

func (l *lowerer) lowerTypeAssertExpression(expr ast.Expression, scope *funcScope, twoValue bool) (ir.Expression, bool) {
	operand, ok := l.lowerOperand(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	if twoValue {
		return ir.Expression{Kind: ir.ExprTypeAssertOK, Type: l.hirType(l.resolveSourceTypeInScope(expr.Type, scope)), Operand: &operand}, true
	}
	return ir.Expression{Kind: ir.ExprTypeAssert, Type: l.hirType(l.resolveSourceTypeInScope(expr.Type, scope)), Operand: &operand}, true
}

func (l *lowerer) isCallableType(typ string) bool {
	typ = l.resolveType(strings.TrimSpace(typ))
	if typ == "Function" {
		return true
	}
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	return ok && view.Shape() == types.Function
}

func (l *lowerer) isInterfaceValueType(typ string) bool {
	typ = l.resolveType(strings.TrimSpace(typ))
	if typ == "Any" {
		return true
	}
	_, ok := l.interfaceType(typ)
	return ok && !l.isGeneralInterfaceType(typ)
}

func (l *lowerer) validateInterfaceConversion(expr ast.Expression, target string, scope *funcScope, span source.Span) bool {
	if isNilLiteral(expr) && l.isNilAssignableType(target) {
		return true
	}
	source := l.expressionType(expr, scope)
	if source == "" || target == "" || !l.isInterfaceValueType(source) || l.isInterfaceValueType(target) {
		return true
	}
	l.add("hirgen.convert.interface", fmt.Sprintf("cannot convert %s to %s; use a type assertion", source, target), span)
	return false
}

func (l *lowerer) validateConversion(expr ast.Expression, target string, scope *funcScope, span source.Span) bool {
	if !l.validateInterfaceConversion(expr, target, scope, span) {
		return false
	}
	if raw, sourceType, ok := l.constValue(expr, scope); ok && l.constRepresentabilityTarget(target) {
		_, _, ok := l.convertTypedConstValue(raw, sourceType, target, span)
		return ok
	}
	if !l.validateStringSliceConversion(expr, target, scope, span) {
		return false
	}
	if !l.validateSliceArrayConversion(expr, target, scope, span) {
		return false
	}
	return true
}

func (l *lowerer) validateStringSliceConversion(expr ast.Expression, target string, scope *funcScope, span source.Span) bool {
	if l.isInterfaceValueType(target) {
		return true
	}
	source := l.expressionType(expr, scope)
	if source == "" || target == "" {
		return true
	}
	sourceIsString := l.isStringType(source)
	targetIsString := l.isStringType(target)
	sourceTextSlice, sourceTextSliceOK := l.textSliceConversionKind(source)
	targetTextSlice, targetTextSliceOK := l.textSliceConversionKind(target)
	if targetIsString {
		if sourceIsString || sourceTextSliceOK || l.isIntegerType(l.resolveNamedUnderlyingType(source)) {
			return true
		}
		l.add("hirgen.convert.string", fmt.Sprintf("cannot convert %s to String", source), span)
		return false
	}
	if targetTextSliceOK {
		if sourceIsString || sourceTextSliceOK && sourceTextSlice == targetTextSlice {
			return true
		}
		l.add("hirgen.convert.string_slice", fmt.Sprintf("cannot convert %s to %s", source, target), span)
		return false
	}
	if sourceIsString {
		l.add("hirgen.convert.string", "cannot convert String to "+target, span)
		return false
	}
	return true
}

func (l *lowerer) textSliceConversionKind(typ string) (string, bool) {
	elem, ok := l.sliceElementType(typ)
	if !ok {
		return "", false
	}
	switch l.typeIdentity(elem) {
	case "Uint8":
		return "byte", true
	case "Int32":
		return "rune", true
	default:
		return "", false
	}
}

func (l *lowerer) validateSliceArrayConversion(expr ast.Expression, target string, scope *funcScope, span source.Span) bool {
	sourceElem, sourceIsSlice := l.sliceElementType(l.expressionType(expr, scope))
	if !sourceIsSlice {
		return true
	}
	targetElem, targetIsArray := l.conversionArrayElementType(target)
	if !targetIsArray {
		return true
	}
	if l.sameTypeIdentity(sourceElem, targetElem) {
		return true
	}
	l.add("hirgen.convert.slice_array_element", fmt.Sprintf("cannot convert slice element %s to array element %s", sourceElem, targetElem), span)
	return false
}

func (l *lowerer) sliceElementType(typ string) (string, bool) {
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	if !ok || view.Shape() != types.Slice {
		return "", false
	}
	elem, ok := view.Elem()
	if !ok {
		return "", false
	}
	return l.typeRefString(elem), true
}

func (l *lowerer) conversionArrayElementType(typ string) (string, bool) {
	if _, elem, ok := l.arrayTypeInfo(typ); ok {
		return elem, true
	}
	if arrayType, ok := l.pointerArrayType(typ); ok {
		_, elem, ok := l.arrayTypeInfo(arrayType)
		return elem, ok
	}
	return "", false
}

func (l *lowerer) isNameBound(name string, span source.Span, scope *funcScope) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if l.isValueNameBound(name, span, scope) {
		return true
	}
	if _, ok := l.dotImportExport(name, span); ok {
		return true
	}
	return false
}

func (l *lowerer) isValueNameBound(name string, span source.Span, scope *funcScope) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if _, ok := l.functions[name]; ok {
		return true
	}
	if _, ok := l.globals[name]; ok {
		return true
	}
	if _, _, ok := l.lookupConstID(name, scope); ok {
		return true
	}
	if _, ok := l.importPathForAlias(name, span); ok {
		return true
	}
	if _, _, ok := l.lookupLocal(name, scope); ok {
		return true
	}
	if _, _, ok := l.lookupUpvalue(name, scope); ok {
		return true
	}
	return false
}

func (l *lowerer) functionResultCount(name string) int {
	if signature, ok := l.semanticFunctionSignature(name); ok {
		return len(signature.Results)
	}
	return 0
}

func (l *lowerer) recordFunctionValueType(scope *funcScope, name, signature string, variadic bool) {
	name = strings.TrimSpace(name)
	variadic = variadic || l.functionTypeVariadic(signature)
	signature = l.resolveNamedUnderlyingType(signature)
	if scope == nil || name == "" {
		return
	}
	params, results, signatureVariadic, ok := l.functionSignatureParts(signature)
	if !ok {
		return
	}
	scope.funcValueResults[name] = len(results)
	scope.funcValueTypes[name] = l.typeRefsFromStrings(results)
	scope.funcValueParams[name] = l.typeRefsFromStrings(params)
	scope.funcValueVarargs[name] = variadic || signatureVariadic
}

func (l *lowerer) functionSignatureResultTypes(signature string) ([]string, bool) {
	_, results, _, ok := l.functionSignatureParts(signature)
	if !ok {
		return nil, false
	}
	return results, true
}

func (l *lowerer) functionSignatureParamTypes(signature string, variadic bool) ([]string, bool, bool) {
	params, _, signatureVariadic, ok := l.functionSignatureParts(signature)
	if !ok {
		return nil, false, false
	}
	return params, variadic || signatureVariadic, true
}

func (l *lowerer) callResultCount(callee ast.Expression, scope *funcScope) int {
	if results, ok := l.callResultTypes(callee, scope); ok {
		return len(results)
	}
	return 1
}

func (l *lowerer) callResultTypes(callee ast.Expression, scope *funcScope) ([]string, bool) {
	switch callee.Kind {
	case ast.ExprFunc:
		return l.resultTypes(callee.Func.Results), true
	case ast.ExprIdent:
		name := strings.TrimSpace(callee.Name)
		if results, ok := l.lookupFunctionValueResultTypes(name, scope); ok {
			return results, true
		}
		if signature, ok := l.semanticFunctionSignature(name); ok {
			return l.signatureResultTypes(signature), true
		}
	}
	if signature, ok := l.semanticExpressionSignature(callee); ok {
		return l.signatureResultTypes(signature), true
	}
	if results, ok := l.functionSignatureResultTypes(l.expressionType(callee, scope)); ok {
		return results, true
	}
	return nil, false
}

func (l *lowerer) callParamTypes(callee ast.Expression, scope *funcScope) ([]string, bool, bool) {
	switch callee.Kind {
	case ast.ExprFunc:
		params, variadic := l.paramTypes(callee.Func.Params)
		return params, variadic, true
	case ast.ExprIdent:
		name := strings.TrimSpace(callee.Name)
		if params, variadic, ok := l.lookupFunctionValueParamTypes(name, scope); ok {
			return params, variadic, true
		}
		if signature, ok := l.semanticFunctionSignature(name); ok {
			return l.signatureParamTypes(signature), signature.Variadic, true
		}
		if typ, ok := l.globalTypes[name]; ok {
			params, _, parsed := l.functionSignatureParamTypes(l.typeRefString(typ), l.globalVariadics[name])
			if parsed {
				return params, l.globalVariadics[name], true
			}
		}
	}
	if signature, ok := l.semanticExpressionSignature(callee); ok {
		return l.signatureParamTypes(signature), signature.Variadic, true
	}
	calleeType := l.expressionType(callee, scope)
	return l.functionSignatureParamTypes(calleeType, l.functionTypeVariadic(calleeType))
}

func (l *lowerer) functionTypeVariadic(typ string) bool {
	typ = l.resolveType(strings.TrimSpace(typ))
	if typ == "" {
		return false
	}
	if _, _, variadic, ok := l.functionSignatureParts(typ); ok {
		return variadic
	}
	if decl, ok := l.typeDeclFor(typ); ok {
		return astFunctionTypeVariadic(decl)
	}
	if export, ok := l.importedTypeInfo(typ); ok {
		return export.Variadic
	}
	return false
}

func (l *lowerer) valueDeclFunctionVariadic(decl ast.ValueDecl, index int, scope *funcScope) bool {
	if typ := l.resolveSourceType(decl.Type); typ != "" {
		return l.functionTypeVariadic(typ)
	}
	if len(decl.Values) == 1 && len(decl.Names) > 1 {
		return l.functionTypeVariadic(l.multiResultValueType(decl.Values[0], index, scope))
	}
	if index < 0 || index >= len(decl.Values) || len(decl.Values) != len(decl.Names) {
		if len(decl.Names) == 1 && len(decl.Values) == 1 {
			index = 0
		} else {
			return false
		}
	}
	return l.expressionFunctionVariadic(decl.Values[index], scope)
}

func (l *lowerer) expressionFunctionVariadic(expr ast.Expression, scope *funcScope) bool {
	if signature, ok := l.semanticExpressionSignature(expr); ok {
		return signature.Variadic
	}
	switch expr.Kind {
	case ast.ExprFunc:
		return funcDeclVariadic(expr.Func)
	case ast.ExprAssert, ast.ExprConvert:
		return astFunctionTypeVariadic(expr.Type) || l.functionTypeVariadic(typeString(expr.Type))
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if signature, ok := l.semanticFunctionSignature(name); ok {
			return signature.Variadic
		}
		if _, variadic, ok := l.lookupFunctionValueParamTypes(name, scope); ok {
			return variadic
		}
		if variadic, ok := l.globalVariadics[name]; ok {
			return variadic
		}
	}
	if typ := l.expressionType(expr, scope); typ != "" {
		return l.functionTypeVariadic(typ)
	}
	return false
}

func (l *lowerer) lookupFunctionValueResultTypes(name string, scope *funcScope) ([]string, bool) {
	name = strings.TrimSpace(name)
	for current := scope; current != nil; current = current.outer {
		if results, ok := current.funcValueTypes[name]; ok {
			return l.typeStringsFromRefs(results), true
		}
	}
	return nil, false
}

func (l *lowerer) lookupFunctionValueParamTypes(name string, scope *funcScope) ([]string, bool, bool) {
	name = strings.TrimSpace(name)
	for current := scope; current != nil; current = current.outer {
		if params, ok := current.funcValueParams[name]; ok {
			return l.typeStringsFromRefs(params), current.funcValueVarargs[name], true
		}
	}
	return nil, false, false
}

func hirResultCount(expr ir.Expression) int {
	switch expr.Kind {
	case ir.ExprCallDirect, ir.ExprCallFFI, ir.ExprCallIntrinsic, ir.ExprCallValue, ir.ExprCallInterface:
		return expr.ResultCount
	case ir.ExprChanRecvOK, ir.ExprChanTryRecv:
		return 2
	case ir.ExprLoadIndexOK, ir.ExprTypeAssertOK:
		return 2
	case ir.ExprLet:
		if expr.Body == nil {
			return 0
		}
		return hirResultCount(*expr.Body)
	case ir.ExprDelete, ir.ExprClear, ir.ExprChanClose:
		return 0
	default:
		return 1
	}
}

func hirResultsCount(expressions []ir.Expression) int {
	count := 0
	for _, expr := range expressions {
		count += hirResultCount(expr)
	}
	return count
}

type callArgumentPlan struct {
	values  []ir.Expression
	binding *multiResultBinding
}

func (plan callArgumentPlan) expressions(prefix []ir.Expression) []ir.Expression {
	out := append([]ir.Expression(nil), prefix...)
	if plan.binding != nil {
		return append(out, plan.binding.expression(plan.values))
	}
	return append(out, plan.values...)
}

func (plan callArgumentPlan) prelude() []ir.Statement {
	if plan.binding == nil {
		return nil
	}
	return []ir.Statement{plan.binding.capture()}
}

func (l *lowerer) lowerCallArguments(expr ast.Expression, scope *funcScope, paramTypes []string, variadic bool, prefix []ir.Expression) ([]ir.Expression, bool) {
	plan, ok := l.planCallArguments(expr, scope, paramTypes, variadic)
	if !ok {
		return nil, false
	}
	return plan.expressions(prefix), true
}

func (l *lowerer) planCallArguments(expr ast.Expression, scope *funcScope, paramTypes []string, variadic bool) (callArgumentPlan, bool) {
	if len(expr.Args) == 1 {
		count := l.expressionResultCount(expr.Args[0], scope)
		if count != 1 {
			if count <= 0 {
				l.add("hirgen.call.multi_result", "call argument expression does not produce a value", expr.Args[0].Span)
				return callArgumentPlan{}, false
			}
			if expr.Ellipsis {
				l.add("hirgen.call.ellipsis", "ellipsis cannot be applied to a multi-result argument", expr.Span)
				return callArgumentPlan{}, false
			}
			targetTypes := make([]string, count)
			if !variadic && count != len(paramTypes) {
				l.add("hirgen.call.arg_count", "multi-result argument count does not match parameters", expr.Span)
				return callArgumentPlan{}, false
			}
			fixedCount := len(paramTypes)
			if variadic {
				fixedCount--
				if count < fixedCount {
					l.add("hirgen.call.arg_count", "multi-result argument is missing fixed parameters", expr.Span)
					return callArgumentPlan{}, false
				}
			}
			for i := range targetTypes {
				targetTypes[i] = l.callArgumentTargetType(i, false, paramTypes, variadic)
			}
			value, ok := l.lowerMultiResultValue(expr.Args[0], count, scope, "hirgen.call.arg_count")
			if !ok {
				return callArgumentPlan{}, false
			}
			binding, ok := l.bindMultiResult(value, targetTypes, scope, "hirgen.assign.type", expr.Args[0])
			if !ok {
				return callArgumentPlan{}, false
			}
			args, ok := l.packCallArguments(expr, binding.values, paramTypes, variadic, nil)
			if !ok {
				return callArgumentPlan{}, false
			}
			return callArgumentPlan{values: args, binding: &binding}, true
		}
	}
	for _, arg := range expr.Args {
		if l.expressionResultCount(arg, scope) != 1 {
			l.add("hirgen.call.multi_result", "multi-result expression must be the only call argument", arg.Span)
			return callArgumentPlan{}, false
		}
	}
	if len(paramTypes) == 0 {
		args, ok := l.lowerExpressions(expr.Args, scope)
		if !ok {
			return callArgumentPlan{}, false
		}
		args, ok = l.packCallArguments(expr, args, paramTypes, variadic, nil)
		return callArgumentPlan{values: args}, ok
	}
	args := make([]ir.Expression, 0, len(expr.Args))
	if !variadic {
		if len(expr.Args) == len(paramTypes) {
			for i, arg := range expr.Args {
				lowered, ok := l.lowerExpressionInType(arg, paramTypes[i], scope)
				if !ok {
					return callArgumentPlan{}, false
				}
				args = append(args, lowered)
			}
		} else {
			var ok bool
			args, ok = l.lowerExpressions(expr.Args, scope)
			if !ok {
				return callArgumentPlan{}, false
			}
		}
		args, ok := l.packCallArguments(expr, args, paramTypes, variadic, nil)
		return callArgumentPlan{values: args}, ok
	}
	for i, arg := range expr.Args {
		targetType := l.callArgumentTargetType(i, expr.Ellipsis, paramTypes, variadic)
		lowered, ok := l.lowerExpressionInType(arg, targetType, scope)
		if !ok {
			return callArgumentPlan{}, false
		}
		args = append(args, lowered)
	}
	args, ok := l.packCallArguments(expr, args, paramTypes, variadic, nil)
	return callArgumentPlan{values: args}, ok
}

func (l *lowerer) callArgumentTargetType(index int, ellipsis bool, paramTypes []string, variadic bool) string {
	if len(paramTypes) == 0 {
		return ""
	}
	if !variadic {
		if index >= 0 && index < len(paramTypes) {
			return paramTypes[index]
		}
		return ""
	}
	fixedCount := len(paramTypes) - 1
	if index < fixedCount {
		return paramTypes[index]
	}
	if ellipsis {
		return paramTypes[len(paramTypes)-1]
	}
	return l.indexElementType(paramTypes[len(paramTypes)-1])
}

func (l *lowerer) packCallArguments(expr ast.Expression, args []ir.Expression, paramTypes []string, variadic bool, prefix []ir.Expression) ([]ir.Expression, bool) {
	if !variadic {
		if expr.Ellipsis {
			l.add("hirgen.call.ellipsis", "ellipsis call requires a variadic function", expr.Span)
			return nil, false
		}
		if hirResultsCount(args) != len(paramTypes) {
			l.add("hirgen.call.arg_count", "call argument count does not match parameters", expr.Span)
			return nil, false
		}
		return append(append([]ir.Expression(nil), prefix...), args...), true
	}
	if len(paramTypes) == 0 {
		l.add("hirgen.call.variadic.shape", "variadic function requires at least one parameter", expr.Span)
		return nil, false
	}
	fixedCount := len(paramTypes) - 1
	if expr.Ellipsis {
		if len(args) != len(paramTypes) {
			l.add("hirgen.call.ellipsis.arg_count", "ellipsis call must provide fixed arguments plus one variadic slice", expr.Span)
			return nil, false
		}
		return append(append([]ir.Expression(nil), prefix...), args...), true
	}
	if len(args) < fixedCount {
		l.add("hirgen.call.arg_count", "variadic call is missing fixed arguments", expr.Span)
		return nil, false
	}
	out := append([]ir.Expression(nil), prefix...)
	out = append(out, args[:fixedCount]...)
	elements := args[fixedCount:]
	arrayType := strings.TrimSpace(paramTypes[len(paramTypes)-1])
	if arrayType == "" {
		arrayType = "Slice<Any>"
	}
	out = append(out, ir.Expression{Kind: ir.ExprSequence, Type: l.hirType(arrayType), Elements: elements})
	return out, true
}
