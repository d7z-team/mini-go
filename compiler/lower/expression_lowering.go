package lower

import (
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	bytecode "github.com/d7z-team/mini-go/runtime/bytecode"
)

func (l *lowerer) lowerExpression(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	lowered, ok := l.lowerExpressionBody(expr, scope)
	if ok && l.semantic != nil {
		if info, found := l.semantic.Exprs[expr.NodeID]; found {
			lowered.ResultTypes = append(lowered.ResultTypes[:0:0], info.Results...)
			if len(lowered.ResultTypes) == 0 && info.Type.Valid() && hirResultCount(lowered) == 1 {
				lowered.ResultTypes = append(lowered.ResultTypes, info.Type)
			}
		}
	}
	return lowered, ok
}

func (l *lowerer) lowerExpressionBody(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	switch expr.Kind {
	case ast.ExprEmbed:
		return l.lowerEmbedInitializer(expr)
	case ast.ExprIdent:
		if resolved, _, ok := l.lookupLocalOrConst(expr.Name, scope); ok {
			if resolved.Kind == ir.ExprConst {
				if value, found := l.lookupConstValue(expr.Name, scope); found && value.Untyped {
					return l.lowerRuntimeConstant(expr, value.Value, value.Type, scope)
				}
			}
			return resolved, true
		}
		if upvalue, ok := l.resolveUpvalue(expr.Name, scope); ok {
			return ir.Expression{Kind: ir.ExprUpvalue, Upvalue: upvalue}, true
		}
		if constant, _, ok := l.lookupConstID(expr.Name, scope); ok {
			if value, found := l.lookupConstValue(expr.Name, scope); found && value.Untyped {
				return l.lowerRuntimeConstant(expr, value.Value, value.Type, scope)
			}
			return ir.Expression{Kind: ir.ExprConst, ConstantID: constant}, true
		}
		if global, ok := l.globals[expr.Name]; ok {
			return ir.Expression{Kind: ir.ExprGlobal, Global: global}, true
		}
		if fn, ok := l.functions[expr.Name]; ok {
			if l.semantic != nil {
				info, found := l.semantic.Exprs[expr.NodeID]
				if !found {
					return ir.Expression{Kind: ir.ExprFunction, Function: fn}, true
				}
				if object, found := l.semantic.Object(info.Object); found && object.Scope == l.semantic.PackageScope {
					if descriptor, intrinsic := bytecode.IntrinsicForSource(l.modulePath, object.Name); intrinsic {
						l.add("hirgen.intrinsic.escape", "intrinsic "+string(descriptor.ID)+" must be called directly", expr.Span)
						return ir.Expression{}, false
					}
				}
			}
			return ir.Expression{Kind: ir.ExprFunction, Function: fn}, true
		}
		if export, ok := l.dotImportExport(expr.Name, expr.Span); ok {
			if export.Kind == check.ObjectType {
				l.add("hirgen.ident.type.value", "type name cannot be used as a value", expr.Span)
				return ir.Expression{}, false
			}
			if export.Kind == check.ObjectConst && export.Untyped {
				return l.lowerRuntimeConstant(expr, export.Value, export.Type, scope)
			}
			return ir.Expression{Kind: ir.ExprLoadExport, ModulePath: export.ModulePath, Export: expr.Name}, true
		}
		l.add("hirgen.ident.unknown", "unknown identifier", expr.Span)
		return ir.Expression{}, false
	case ast.ExprLiteral:
		raw, typ, ok := literalValue(expr)
		if !ok {
			l.add("hirgen.literal.invalid", "invalid literal value", expr.Span)
			return ir.Expression{}, false
		}
		return l.lowerRuntimeConstant(expr, raw, typ, scope)
	case ast.ExprUnary:
		if call, overloaded := l.lowerOperatorCall(expr, scope); overloaded {
			return call, true
		}
		if raw, typ, ok := l.constValue(expr, scope); ok {
			return l.lowerRuntimeConstant(expr, raw, typ, scope)
		}
		operand, ok := l.lowerOperand(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprUnary, Operator: expr.Operator, Operand: &operand}, true
	case ast.ExprBinary:
		if call, overloaded := l.lowerOperatorCall(expr, scope); overloaded {
			return call, true
		}
		if l.suppressConstantArithmetic == 0 && !l.validateConstantArithmetic(expr, scope) {
			return ir.Expression{}, false
		}
		if raw, typ, ok := l.constValue(expr, scope); ok {
			return l.lowerRuntimeConstant(expr, raw, typ, scope)
		}
		left, right, ok := l.lowerBinaryOperands(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprBinary, Operator: expr.Operator, Type: l.hirType(l.binaryExpressionType(expr, scope)), Left: &left, Right: &right}, true
	case ast.ExprCall:
		if id, ok := l.semantic.IntrinsicCalls[expr.NodeID]; ok {
			descriptor, found := bytecode.Intrinsic(id)
			if !found {
				l.add("hirgen.intrinsic.unknown", "unknown compiler intrinsic", expr.Span)
				return ir.Expression{}, false
			}
			args, lowered := l.lowerExpressions(expr.Args, scope)
			if !lowered {
				return ir.Expression{}, false
			}
			if id == "ffi.call" {
				return ir.Expression{Kind: ir.ExprCallFFI, Args: args, ResultCount: descriptor.ResultCount}, true
			}
			return ir.Expression{Kind: ir.ExprCallIntrinsic, Intrinsic: string(id), Args: args, ResultCount: descriptor.ResultCount}, true
		}
		if expr.Callee == nil {
			l.add("hirgen.call.callee.missing", "missing call callee", expr.Span)
			return ir.Expression{}, false
		}
		if target, ok := l.typeConversionCallTarget(expr, scope); ok {
			if expr.Ellipsis {
				l.add("hirgen.convert.ellipsis", "type conversion cannot use ellipsis", expr.Span)
				return ir.Expression{}, false
			}
			if len(expr.Args) != 1 {
				l.add("hirgen.convert.arg_count", "type conversion requires exactly one argument", expr.Span)
				return ir.Expression{}, false
			}
			if isNilLiteral(expr.Args[0]) && l.isNilAssignableType(target) {
				return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(target), Value: json.RawMessage("null")}, true
			}
			if !l.validateConversion(expr.Args[0], target, scope, expr.Span) {
				return ir.Expression{}, false
			}
			if raw, sourceType, ok := l.constValue(expr.Args[0], scope); ok && l.constRepresentabilityTarget(target) {
				converted, typ, ok := l.convertTypedConstValue(raw, sourceType, target, expr.Span)
				if !ok {
					return ir.Expression{}, false
				}
				return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(typ), Value: converted}, true
			}
			operand, ok := l.lowerExpression(expr.Args[0], scope)
			if !ok {
				return ir.Expression{}, false
			}
			return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType(target), Operand: &operand}, true
		}
		if expr.Callee.Kind == ast.ExprIdent && l.isBuiltinCallName(expr.Callee.Name, expr.Callee.Span, scope) {
			return l.lowerBuiltinCall(expr, scope)
		}
		if expr.Callee.Kind == ast.ExprSelector {
			if isBlankIdentifier(expr.Callee.Field) {
				l.add("hirgen.selector.blank", "selector field cannot be blank", expr.Callee.Span)
				return ir.Expression{}, false
			}
			if converted, ok := l.lowerModuleTypeConversionCall(expr, scope); ok {
				return converted, true
			}
			if l.rejectAmbiguousSelector(*expr.Callee, scope, "hirgen.method.ambiguous") {
				return ir.Expression{}, false
			}
			if call, ok := l.lowerMethodCall(expr, scope); ok {
				return call, true
			}
			if selection, ok := l.semanticSelection(*expr.Callee); ok && selection.Kind == check.SelectionPackageMember && expr.Callee != nil {
				signature, found := l.semanticCallSignature(expr)
				if !found {
					l.add("hirgen.semantic.call", "dependency function call is missing semantic signature", expr.Span)
					return ir.Expression{}, false
				}
				args, ok := l.lowerCallArguments(expr, scope, l.signatureParamTypes(signature), signature.Variadic, nil)
				if !ok {
					return ir.Expression{}, false
				}
				callee, ok := l.lowerExpression(*expr.Callee, scope)
				if !ok {
					return ir.Expression{}, false
				}
				return ir.Expression{
					Kind:        ir.ExprCallValue,
					Operand:     &callee,
					Args:        args,
					ResultCount: len(signature.Results), ResultTypes: signature.Results,
				}, true
			}
		}
		if expr.Callee.Kind == ast.ExprIdent {
			if fn, ok := l.functions[expr.Callee.Name]; ok {
				signature, found := l.semanticCallSignature(expr)
				if !found {
					l.add("hirgen.semantic.call", "direct function call is missing semantic signature", expr.Span)
					return ir.Expression{}, false
				}
				args, ok := l.lowerCallArguments(expr, scope, l.signatureParamTypes(signature), signature.Variadic, nil)
				if !ok {
					return ir.Expression{}, false
				}
				return ir.Expression{
					Kind:        ir.ExprCallDirect,
					Function:    fn,
					Args:        args,
					ResultCount: len(signature.Results), ResultTypes: signature.Results,
				}, true
			}
		}
		calleeType := l.expressionType(*expr.Callee, scope)
		if calleeType != "" && !l.isCallableType(calleeType) {
			l.add("hirgen.call.type", "cannot call value of type "+calleeType, expr.Callee.Span)
			return ir.Expression{}, false
		}
		var args []ir.Expression
		var ok bool
		if paramTypes, variadic, found := l.callParamTypes(*expr.Callee, scope); found {
			args, ok = l.lowerCallArguments(expr, scope, paramTypes, variadic, nil)
		}
		if expr.Ellipsis && args == nil {
			l.add("hirgen.call.ellipsis", "ellipsis call requires a known Mini-Go variadic function", expr.Span)
			return ir.Expression{}, false
		}
		if args == nil {
			args, ok = l.lowerExpressions(expr.Args, scope)
		}
		if !ok {
			return ir.Expression{}, false
		}
		callee, ok := l.lowerExpression(*expr.Callee, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprCallValue, Operand: &callee, Args: args, ResultCount: l.callResultCount(*expr.Callee, scope)}, true
	case ast.ExprSelector:
		if isBlankIdentifier(expr.Field) {
			l.add("hirgen.selector.blank", "selector field cannot be blank", expr.Span)
			return ir.Expression{}, false
		}
		if l.rejectAmbiguousSelector(expr, scope, "hirgen.selector.ambiguous") {
			return ir.Expression{}, false
		}
		if value, ok := l.lowerMethodExpression(expr); ok {
			return value, true
		}
		if value, ok := l.lowerInterfaceMethodExpression(expr); ok {
			return value, true
		}
		if value, ok := l.lowerMethodValue(expr, scope); ok {
			return value, true
		}
		if _, ok := l.methodExpressionReceiverType(*expr.Operand, scope); ok {
			l.add("hirgen.method_expression.method", "method expression requires a visible method in receiver type method set", expr.Span)
			return ir.Expression{}, false
		}
		if modulePath, ok := l.selectorModulePath(expr, scope); ok {
			if selection, found := l.semanticSelection(expr); !found || selection.Kind != check.SelectionPackageMember {
				l.add("hirgen.semantic.selector", "imported member is missing semantic selection", expr.Span)
				return ir.Expression{}, false
			}
			l.markImportExport(modulePath, expr.Field)
			if export, ok := l.selectorExport(expr, scope); ok {
				l.markImportExportsInType(modulePath, export.Type)
				l.markImportExportsInType(modulePath, export.Underlying)
				if export.Kind == check.ObjectConst && export.Untyped {
					return l.lowerRuntimeConstant(expr, export.Value, export.Type, scope)
				}
			}
			return ir.Expression{Kind: ir.ExprLoadExport, ModulePath: modulePath, Export: expr.Field}, true
		}
		if selection, ok := l.semanticSelection(expr); ok && selection.Kind == check.SelectionField && len(selection.Index) > 1 {
			path, _, pathOK := l.semanticSelectionPath(selection)
			if !pathOK {
				l.add("hirgen.semantic.selector", "field selector is missing semantic path", expr.Span)
				return ir.Expression{}, false
			}
			operand, lowered := l.lowerExpression(*expr.Operand, scope)
			if !lowered {
				return ir.Expression{}, false
			}
			return projectSelectorPath(operand, path), true
		}
		operand, ok := l.lowerOperand(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprLoadField, Operand: &operand, Field: expr.Field}, true
	case ast.ExprIndex:
		if !l.validateConstantIndexRange(expr, scope) {
			return ir.Expression{}, false
		}
		operand, ok := l.lowerOperand(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		keyType := "Int"
		if expr.Operand != nil {
			if !l.validateMapKeyComparable(l.expressionType(*expr.Operand, scope), expr.Span) {
				return ir.Expression{}, false
			}
			if mapKeyType := l.mapIndexKeyType(*expr.Operand, scope); mapKeyType != "" {
				keyType = mapKeyType
			}
		}
		index, ok := l.lowerIndexExpression(*expr.Index, keyType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprLoadIndex, Operand: &operand, Index: &index}, true
	case ast.ExprSlice:
		if !l.validateFullSliceExpression(expr, scope) {
			return ir.Expression{}, false
		}
		objectType := l.expressionType(*expr.Operand, scope)
		var operand ir.Expression
		var ok bool
		if l.isArrayType(objectType) {
			_, category, structured := l.semanticValueFact(*expr.Operand)
			if structured && !category.Addressable() {
				return ir.Expression{}, false
			}
			target, addressable := l.lowerAddressTarget(*expr.Operand, scope)
			if !addressable {
				return ir.Expression{}, false
			}
			pointerType := "Ptr<" + objectType + ">"
			target.expr.Type = l.hirType(pointerType)
			operand = wrapAddressLets(target.expr, target.lets)
			operand.Type = l.hirType(pointerType)
		} else {
			operand, ok = l.lowerOperand(expr, scope)
			if !ok {
				return ir.Expression{}, false
			}
		}
		zero := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: json.RawMessage(`0`)}
		noMax := ir.Expression{Kind: ir.ExprZero, Type: l.hirType("Void")}
		start := zero
		if expr.Start != nil {
			start, ok = l.lowerIndexExpression(*expr.Start, "Int", scope)
			if !ok {
				return ir.Expression{}, false
			}
		}
		maxIndex := noMax
		if expr.Max != nil {
			maxIndex, ok = l.lowerIndexExpression(*expr.Max, "Int", scope)
			if !ok {
				return ir.Expression{}, false
			}
		}
		if expr.End == nil {
			operandType := firstNonEmpty(l.typeRefString(operand.Type), objectType)
			objectLocal := l.newSyntheticLocal(scope, "slice.object", operandType)
			objectRef := ir.Expression{Kind: ir.ExprLocal, Local: objectLocal, Type: l.hirType(operandType)}
			end := ir.Expression{Kind: ir.ExprLen, Operand: &objectRef}
			body := ir.Expression{Kind: ir.ExprSlice, Operand: &objectRef, Start: &start, End: &end, Max: &maxIndex}
			return ir.Expression{Kind: ir.ExprLet, Local: objectLocal, Bind: &operand, Body: &body}, true
		}
		end, ok := l.lowerIndexExpression(*expr.End, "Int", scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprSlice, Operand: &operand, Start: &start, End: &end, Max: &maxIndex}, true
	case ast.ExprComposite:
		if expr.Type == nil || expr.Type.Kind == ast.TypeInvalid {
			l.add("hirgen.composite.elided_context", "elided composite literal type is only valid inside another composite literal", expr.Span)
			return ir.Expression{}, false
		}
		return l.lowerComposite(expr, scope)
	case ast.ExprFunc:
		return l.lowerFuncLiteral(expr, scope)
	case ast.ExprAddr:
		if expr.Operand == nil {
			l.add("hirgen.addr.target", "address-of requires an operand", expr.Span)
			return ir.Expression{}, false
		}
		if expr.Operand.Kind == ast.ExprComposite {
			value, ok := l.lowerExpression(*expr.Operand, scope)
			if !ok {
				return ir.Expression{}, false
			}
			local := l.newSyntheticLocal(scope, "addr.object", l.expressionType(*expr.Operand, scope))
			addr := ir.Expression{Kind: ir.ExprAddressOf, Local: local}
			return ir.Expression{Kind: ir.ExprLet, Local: local, Bind: &value, Body: &addr}, true
		}
		target, ok := l.lowerAddressTarget(*expr.Operand, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return wrapAddressLets(target.expr, target.lets), true
	case ast.ExprDeref:
		operand, ok := l.lowerOperand(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprLoadIndirect, Operand: &operand}, true
	case ast.ExprReceive:
		return l.lowerReceiveExpression(expr, scope, false)
	case ast.ExprConvert:
		target := l.resolveSourceTypePtr(expr.Type, scope)
		if expr.Operand != nil && isNilLiteral(*expr.Operand) && l.isNilAssignableType(target) {
			return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(target), Value: json.RawMessage("null")}, true
		}
		if expr.Operand != nil && !l.validateConversion(*expr.Operand, target, scope, expr.Span) {
			return ir.Expression{}, false
		}
		if expr.Operand != nil {
			if raw, sourceType, ok := l.constValue(*expr.Operand, scope); ok && l.constRepresentabilityTarget(target) {
				converted, typ, ok := l.convertTypedConstValue(raw, sourceType, target, expr.Span)
				if !ok {
					return ir.Expression{}, false
				}
				return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(typ), Value: converted}, true
			}
		}
		operand, ok := l.lowerOperand(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType(target), Operand: &operand}, true
	case ast.ExprAssert:
		return l.lowerTypeAssertExpression(expr, scope, false)
	default:
		l.add("hirgen.expr.unsupported", "unsupported AST expression for HIR emit", expr.Span)
		return ir.Expression{}, false
	}
}
