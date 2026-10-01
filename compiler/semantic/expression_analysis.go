package semantic

import (
	"strconv"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/types"
	bytecode "github.com/d7z-team/mini-go/runtime/bytecode"
)

func (a *analyzer) analyzeExpr(expr *ast.Expression, scope ScopeID) {
	if expr == nil || expr.Kind == ast.ExprInvalid {
		return
	}
	a.info.NodeScopes[expr.NodeID] = scope
	info := ExprInfo{Mode: ExprValue, Category: Value}
	if expr.Type != nil {
		info.Type = a.typeOf(*expr.Type)
	}
	if expr.Kind == ast.ExprIdent {
		if expr.Name == "_" {
			info.Category = ValueBlank
		}
		if object, ok := a.info.Lookup(scope, expr.Name); ok {
			if object.Kind == ObjectVar && !object.Type.Valid() {
				a.inferVariableObject(object.ID)
				object = a.info.Objects[object.ID]
			}
			a.info.Uses[expr.NodeID] = object.ID
			info.Object = object.ID
			info.Type = object.Type
			info.Untyped = object.Untyped
			switch object.Kind {
			case ObjectType, ObjectTypeParam:
				info.Mode = ExprType
			case ObjectBuiltin:
				info.Mode = ExprBuiltin
			case ObjectImport:
				info.Mode = ExprPackage
			case ObjectConst:
				info.Mode = ExprConstant
			}
			if object.Kind == ObjectVar {
				info.Category = ValueAddressable
			}
		}
	}
	a.info.Exprs[expr.NodeID] = info
	a.analyzeType(expr.Type, scope)
	a.analyzeExpr(expr.Left, scope)
	a.analyzeExpr(expr.Right, scope)
	a.analyzeExpr(expr.Operand, scope)
	a.analyzeExpr(expr.Callee, scope)
	for i := range expr.Args {
		a.analyzeExpr(&expr.Args[i], scope)
	}
	a.analyzeExpr(expr.Index, scope)
	a.analyzeExpr(expr.Start, scope)
	a.analyzeExpr(expr.End, scope)
	a.analyzeExpr(expr.Max, scope)
	if expr.Kind == ast.ExprComposite {
		a.analyzeCompositeElements(expr, scope)
	}
	if expr.Kind == ast.ExprFunc && expr.Func != nil {
		a.analyzeFunc(expr.Func, scope, "")
	}
	if expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList {
		a.classifyBracket(expr)
	}
	if expr.Kind == ast.ExprCall && expr.Callee != nil {
		if instance, ok := a.info.Instances[expr.Callee.NodeID]; ok {
			a.info.Calls[expr.NodeID] = CallInfo{
				Kind: CallFunction, Callee: instance.Generic,
				TypeArgs: append([]types.TypeRef(nil), instance.TypeArgs...),
			}
		}
	}
	a.finalizeExpr(expr)
}

func (a *analyzer) finalizeExpr(expr *ast.Expression) {
	info := a.info.Exprs[expr.NodeID]
	switch expr.Kind {
	case ast.ExprIdent:
		if signature, ok := a.info.Relations.View(info.Type).Function(); ok && validFunctionSignature(signature) {
			info.Signature = &signature
		}
	case ast.ExprLiteral:
		info = a.finalizeLiteral(expr, info)
	case ast.ExprUnary:
		info = a.finalizeUnary(expr, info)
	case ast.ExprBinary:
		info = a.finalizeBinary(expr, info)
	case ast.ExprFunc:
		if expr.Func == nil {
			break
		}
		signature := a.functionTypeSignature(expr.Func.Params, expr.Func.Results)
		if validFunctionSignature(signature) {
			info.Type = a.storeFunctionType(expr.NodeID, "function", signature)
			info.Signature = &signature
			info.Mode = ExprValue
		}
	case ast.ExprComposite:
		composite := a.compositeInfo(expr)
		a.info.Composites[expr.NodeID] = composite
		if composite.Type.Valid() {
			info.Type = composite.Type
			info.Mode = ExprValue
		}
	case ast.ExprConvert, ast.ExprAssert:
		if typ := a.resolvedTypePtr(expr.Type); typ.Valid() {
			info.Type = typ
			info.Mode = ExprValue
			if expr.Kind == ast.ExprConvert && expr.Operand != nil && a.info.Exprs[expr.Operand.NodeID].Mode == ExprConstant {
				if _, primitive := a.info.Relations.View(typ).Primitive(); primitive {
					info.Mode = ExprConstant
				}
			}
			if expr.Kind == ast.ExprConvert {
				a.validateGenericConversion(expr, typ)
			}
		}
	case ast.ExprEmbed:
		// Embed initializers are validated with their declaration target.
	case ast.ExprAddr:
		if expr.Operand != nil {
			operand := a.info.Exprs[expr.Operand.NodeID]
			if operand.Type.Valid() {
				if !operand.Category.Addressable() && expr.Operand.Kind != ast.ExprComposite {
					a.addDiagnostic("semantic.address.target", "address operand is not addressable", expr.Operand.Span)
				}
				info.Type = a.storeDerivedType(expr.NodeID, "address", types.Pointer, operand.Type)
				info.Mode = ExprValue
				info.Untyped = false
			}
		}
	case ast.ExprDeref:
		if expr.Operand != nil {
			operand := a.info.Exprs[expr.Operand.NodeID]
			if operand.Type.Kind == types.TypeParameter && a.typeParameterInScope(operand.Type, expr.NodeID) {
				if elem, ok := a.typeParameterPointerElem(operand.Type); ok {
					info.Type = elem
					info.Mode = operand.Mode
					info.Category = ValueAddressable
				} else if !a.deferTypeParameterCheck(operand.Type) {
					a.addDiagnostic("semantic.generic.deref_constraint", "type parameter constraint does not permit dereference", expr.Span)
				}
				break
			}
			if operand.Mode == ExprType && operand.Type.Valid() {
				id := types.TypeID("pointer_type." + strconv.FormatUint(uint64(expr.NodeID), 10))
				node := types.TypeNode{ID: id, Kind: types.Pointer, Elem: operand.Type}
				_ = a.info.TypeTable.Add(node)
				info.Type = types.TypeRef{Kind: types.Pointer, Node: id}
				info.Mode = ExprType
				break
			}
			if elem, ok := a.info.Relations.View(operand.Type).Elem(); ok && a.info.Relations.View(operand.Type).Shape() == types.Pointer {
				info.Type = elem
				info.Mode = operand.Mode
				if operand.Mode != ExprType {
					info.Category = ValueAddressable
				}
			} else if operand.Type.Valid() && a.info.TypeExact(operand.Type) {
				a.addDiagnostic("semantic.deref.type", "cannot dereference a non-pointer value", expr.Span)
			}
		}
	case ast.ExprIndex:
		if expr.Operand != nil {
			operand := a.info.Exprs[expr.Operand.NodeID]
			if operand.Type.Kind == types.TypeParameter && a.typeParameterInScope(operand.Type, expr.NodeID) {
				if _, elem, ok := a.typeParameterIndexTypes(operand.Type); ok {
					info.Type = elem
					info.Category = ValueAddressable
					terms, _ := a.typeParameterTerms(operand.Type)
					for _, term := range terms {
						view := a.info.Relations.View(term)
						switch view.Shape() {
						case types.Map:
							if info.Category != Value {
								info.Category = ValueMapIndex
							}
						case types.Array:
							if !operand.Category.Addressable() {
								info.Category = Value
							}
						case types.Slice, types.Pointer:
						default:
							info.Category = Value
						}
					}
				} else if !a.deferTypeParameterCheck(operand.Type) {
					a.addDiagnostic("semantic.generic.index_constraint", "type parameter constraint does not permit indexing", expr.Span)
				}
				break
			}
			view := a.info.Relations.View(operand.Type)
			if expr.Index != nil {
				if key, _, ok := view.Map(); ok {
					a.validateAssignments([]ast.Expression{*expr.Index}, []types.TypeRef{key})
				} else if value, ok := a.evaluateConstantExpression(*expr.Index, a.info.NodeScopes[expr.NodeID]); ok && a.info.TypeExact(operand.Type) && (view.Shape() == types.Array || view.Shape() == types.Slice || view.Shape() == types.Pointer || view.Shape() == types.Primitive) {
					rational, valid := value.Rational()
					integer, integral := rational.Integer()
					index, fits := constant.SignedDecimalInt64(integer)
					bound, _, bounded := view.Array()
					if primitive, ok := view.Primitive(); ok && primitive == types.PrimitiveString {
						if textValue, ok := a.evaluateConstantExpression(*expr.Operand, a.info.NodeScopes[expr.NodeID]); ok {
							if text, err := strconv.Unquote(textValue.Text); err == nil {
								bound, bounded = int64(len(text)), true
							}
						}
					}
					if elem, pointer := view.Elem(); pointer && view.Shape() == types.Pointer {
						bound, _, bounded = a.info.Relations.View(elem).Array()
					}
					if !valid || !integral || !fits || index < 0 || bounded && bound >= 0 && index >= bound {
						a.addDiagnostic("semantic.index.range", "constant index is outside the indexed value", expr.Index.Span)
					}
				}
			}
			if _, elem, ok := view.Array(); ok {
				info.Type = elem
				if operand.Category.Addressable() {
					info.Category = ValueAddressable
				}
			} else if key, elem, ok := view.Map(); ok && key.Valid() {
				info.Type = elem
				info.Category = ValueMapIndex
			} else if elem, ok := view.Elem(); ok && view.Shape() == types.Slice {
				info.Type = elem
				info.Category = ValueAddressable
			} else if elem, ok := view.Elem(); ok && view.Shape() == types.Pointer {
				if _, arrayElem, arrayOK := a.info.Relations.View(elem).Array(); arrayOK {
					info.Type = arrayElem
					info.Category = ValueAddressable
				}
			} else if primitive, ok := view.Primitive(); ok && primitive == types.PrimitiveString {
				info.Type = types.Builtin(types.PrimitiveUint8)
			}
		}
	case ast.ExprSlice:
		if expr.Operand != nil {
			operand := a.info.Exprs[expr.Operand.NodeID]
			if operand.Type.Kind == types.TypeParameter && a.typeParameterInScope(operand.Type, expr.NodeID) {
				if a.typeParameterSliceable(operand.Type) {
					info.Type = operand.Type
				} else if !a.deferTypeParameterCheck(operand.Type) {
					a.addDiagnostic("semantic.generic.slice_constraint", "type parameter constraint does not permit slicing", expr.Span)
				}
				break
			}
			view := a.info.Relations.View(operand.Type)
			if view.Shape() == types.Slice {
				info.Type = operand.Type
			} else if _, elem, ok := view.Array(); ok {
				if !operand.Category.Addressable() {
					a.addDiagnostic("semantic.slice.array_addressable", "cannot slice an unaddressable array value", expr.Operand.Span)
				}
				id := types.TypeID("slice." + strconv.FormatUint(uint64(expr.NodeID), 10))
				node := types.TypeNode{ID: id, Kind: types.Slice, Elem: elem}
				_ = a.info.TypeTable.Add(node)
				info.Type = types.TypeRef{Kind: types.Slice, Node: id}
			} else if elem, ok := view.Elem(); ok && view.Shape() == types.Pointer {
				if _, arrayElem, arrayOK := a.info.Relations.View(elem).Array(); arrayOK {
					id := types.TypeID("slice." + strconv.FormatUint(uint64(expr.NodeID), 10))
					node := types.TypeNode{ID: id, Kind: types.Slice, Elem: arrayElem}
					_ = a.info.TypeTable.Add(node)
					info.Type = types.TypeRef{Kind: types.Slice, Node: id}
				}
			} else if primitive, ok := view.Primitive(); ok && primitive == types.PrimitiveString {
				info.Type = operand.Type
			}
		}
	case ast.ExprReceive:
		if expr.Operand != nil {
			operand := a.info.Exprs[expr.Operand.NodeID]
			if operand.Type.Kind == types.TypeParameter && a.typeParameterInScope(operand.Type, expr.NodeID) {
				if elem, ok := a.typeParameterReceiveElem(operand.Type); ok {
					info.Type = elem
				} else if !a.deferTypeParameterCheck(operand.Type) {
					a.addDiagnostic("semantic.generic.receive_constraint", "type parameter constraint does not permit receive", expr.Span)
				}
				break
			}
			if _, elem, ok := a.info.Relations.View(operand.Type).Waitable(); ok {
				info.Type = elem
			}
		}
	case ast.ExprSelector:
		info = a.finalizeSelector(expr, info)
	case ast.ExprCall:
		if expr.Callee == nil {
			break
		}
		for i := range expr.Args {
			if a.info.Exprs[expr.Args[i].NodeID].Mode == ExprNoValue {
				a.addDiagnostic("semantic.call.argument.no_value", "call argument produces no value", expr.Args[i].Span)
			}
		}
		callee := a.info.Exprs[expr.Callee.NodeID]
		if callee.Mode == ExprBuiltin {
			if object, ok := a.info.Object(callee.Object); ok {
				if (object.Name == "min" || object.Name == "max") && expr.Ellipsis {
					a.addDiagnostic("semantic.builtin.minmax.ellipsis", "min and max do not accept slice expansion", expr.Span)
				}
				info = a.finalizeBuiltinCall(expr, info, object.Name)
				a.info.Calls[expr.NodeID] = CallInfo{Kind: CallBuiltin, Callee: object.ID, Target: info.Type}
				a.validateTypeParameterBuiltin(expr, object.Name)
			}
			break
		}
		if callee.Mode == ExprType {
			info.Type = callee.Type
			info.Mode = ExprValue
			if len(expr.Args) == 1 && a.info.Exprs[expr.Args[0].NodeID].Mode == ExprConstant {
				if _, primitive := a.info.Relations.View(callee.Type).Primitive(); primitive {
					info.Mode = ExprConstant
				}
			}
			info.Results = []types.TypeRef{callee.Type}
			a.info.Calls[expr.NodeID] = CallInfo{Kind: CallConversion, Callee: callee.Object, Target: callee.Type}
			a.validateGenericConversion(expr, callee.Type)
			break
		}
		if callee.Mode == ExprBuiltin || callee.Mode == ExprPackage {
			break
		}
		signature, ok := a.info.Relations.View(callee.Type).Function()
		if !ok || !validFunctionSignature(signature) {
			break
		}
		call := a.info.Calls[expr.NodeID]
		call.Kind = CallFunction
		if selection := a.info.Selections[expr.Callee.NodeID]; selection.Kind == SelectionMethod {
			if selection.Interface {
				call.Kind = CallInterfaceMethod
			} else {
				call.Kind = CallMethod
			}
		}
		call.Callee = callee.Object
		call.Signature = signature
		call.Variadic = signature.Variadic
		targets := make([]types.TypeRef, len(signature.Params))
		for i, param := range signature.Params {
			targets[i] = param.Type
		}
		if signature.Variadic && !expr.Ellipsis && len(targets) != 0 && len(expr.Args) >= len(targets)-1 {
			element, ok := a.info.Relations.View(targets[len(targets)-1]).Elem()
			if ok {
				targets = targets[:len(targets)-1]
				for len(targets) < len(expr.Args) {
					targets = append(targets, element)
				}
			}
		}
		a.validateAssignments(expr.Args, targets)
		if object, found := a.info.Object(callee.Object); found && object.Scope == a.info.PackageScope {
			if descriptor, intrinsic := bytecode.IntrinsicForSource(a.info.ModulePath, object.Name); intrinsic {
				actual := types.FormatSignature(a.info.TypeTable, signature)
				if actual != descriptor.Signature {
					a.addDiagnostic("semantic.intrinsic.signature", "intrinsic "+string(descriptor.ID)+" has signature "+actual+", want "+descriptor.Signature, expr.Span)
				} else {
					call.Kind = CallIntrinsic
					a.info.IntrinsicCalls[expr.NodeID] = descriptor.ID
				}
			}
		}
		a.info.Calls[expr.NodeID] = call
		info.Signature = nil
		info.Results = append([]types.TypeRef(nil), signature.Results...)
		switch len(signature.Results) {
		case 0:
			info.Type = types.VoidType()
			info.Mode = ExprNoValue
		case 1:
			info.Type = signature.Results[0]
			info.Mode = ExprValue
			if resultSignature, ok := a.info.Relations.View(info.Type).Function(); ok {
				info.Signature = &resultSignature
			}
		default:
			info.Type = types.TypeRef{}
			info.Mode = ExprMultiValue
		}
	}
	a.info.Exprs[expr.NodeID] = info
}

func (a *analyzer) expressionListTypes(expressions []ast.Expression) []types.TypeRef {
	if len(expressions) == 1 {
		info := a.info.Exprs[expressions[0].NodeID]
		if len(info.Results) > 1 {
			return append([]types.TypeRef(nil), info.Results...)
		}
	}
	out := make([]types.TypeRef, 0, len(expressions))
	for i := range expressions {
		info := a.info.Exprs[expressions[i].NodeID]
		if info.Type.Valid() && info.Mode != ExprNoValue && info.Mode != ExprMultiValue {
			out = append(out, info.Type)
		} else {
			out = append(out, types.TypeRef{})
		}
	}
	return out
}

func (a *analyzer) assignmentExpressionTypes(expressions []ast.Expression, targets int) []types.TypeRef {
	if len(expressions) > 1 {
		for i := range expressions {
			if a.info.Exprs[expressions[i].NodeID].Mode == ExprMultiValue {
				return nil
			}
		}
	}
	typesList := a.expressionListTypes(expressions)
	if len(expressions) != 1 || targets != 2 || len(typesList) == targets {
		return typesList
	}
	expr := expressions[0]
	info := a.info.Exprs[expr.NodeID]
	if expr.Kind == ast.ExprReceive || expr.Kind == ast.ExprAssert || info.Category == ValueMapIndex {
		return []types.TypeRef{info.Type, types.Builtin(types.PrimitiveBool)}
	}
	return typesList
}

func (a *analyzer) classifyBracket(expr *ast.Expression) {
	if expr.Operand == nil {
		return
	}
	operand := a.info.Exprs[expr.Operand.NodeID]
	if len(a.info.GenericDecls[operand.Object]) == 0 {
		return
	}
	var args []ast.Expression
	if expr.Kind == ast.ExprIndex && expr.Index != nil {
		args = []ast.Expression{*expr.Index}
	} else {
		args = expr.Args
	}
	typeArgs := make([]types.TypeRef, 0, len(args))
	for i := range args {
		arg := a.info.Exprs[args[i].NodeID]
		if arg.Mode != ExprType || !arg.Type.Valid() {
			return
		}
		typeArgs = append(typeArgs, arg.Type)
	}
	a.info.Instances[expr.NodeID] = InstanceInfo{Generic: operand.Object, TypeArgs: typeArgs}
	info := a.info.Exprs[expr.NodeID]
	info.Mode = ExprValue
	info.Object = operand.Object
	a.info.Exprs[expr.NodeID] = info
}
