package semantic

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (a *analyzer) finalizeLiteral(expr *ast.Expression, info ExprInfo) ExprInfo {
	info.Type = a.resolvedTypePtr(expr.Type)
	info.Mode = ExprConstant
	info.Untyped = true
	return info
}

func (a *analyzer) finalizeUnary(expr *ast.Expression, info ExprInfo) ExprInfo {
	if expr.Operand == nil {
		return info
	}
	operand := a.info.Exprs[expr.Operand.NodeID]
	info.Type = operand.Type
	info.Mode = ExprValue
	info.Untyped = false
	if operand.Mode == ExprConstant {
		info.Mode = ExprConstant
		info.Untyped = operand.Untyped
	}
	if strings.TrimSpace(expr.Operator) == "!" {
		info.Type = types.Builtin(types.PrimitiveBool)
	}
	if operand.Type.Kind == types.TypeParameter {
		if !a.typeParameterUnaryAllowed(operand.Type, strings.TrimSpace(expr.Operator)) {
			if resolved, found := a.resolveOperator(expr.NodeID, expr.Operator, operand, ExprInfo{}, expr.Span); found {
				return resolved
			}
		}
		a.validateTypeParameterUnary(expr, operand.Type)
	} else if !a.nativeUnaryAllowed(operand.Type, strings.TrimSpace(expr.Operator)) {
		if resolved, found := a.resolveOperator(expr.NodeID, expr.Operator, operand, ExprInfo{}, expr.Span); found {
			return resolved
		}
	}
	return info
}

func (a *analyzer) nativeUnaryAllowed(ref types.TypeRef, operator string) bool {
	view := a.info.Relations.View(ref)
	numeric, numericOK := view.NumericInfo()
	switch operator {
	case "+", "-":
		return numericOK
	case "^":
		return numericInteger(numeric, numericOK)
	case "!":
		return primitiveIs(view, types.PrimitiveBool)
	default:
		return true
	}
}

func (a *analyzer) typeParameterUnaryAllowed(ref types.TypeRef, operator string) bool {
	terms, ok := a.typeParameterTerms(ref)
	if !ok || len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if !a.nativeUnaryAllowed(term, operator) {
			return false
		}
	}
	return true
}

func (a *analyzer) validateTypeParameterUnary(expr *ast.Expression, ref types.TypeRef) {
	if !a.typeParameterInScope(ref, expr.NodeID) {
		return
	}
	terms, ok := a.typeParameterTerms(ref)
	if !ok {
		if a.deferTypeParameterCheck(ref) {
			return
		}
		a.addDiagnostic("semantic.generic.unary_constraint", "type parameter constraint does not permit this unary operation", expr.Span)
		return
	}
	operator := strings.TrimSpace(expr.Operator)
	for _, term := range terms {
		view := a.info.Relations.View(term)
		numeric, numericOK := view.NumericInfo()
		var allowed bool
		switch operator {
		case "+", "-":
			allowed = numericOK
		case "^":
			allowed = numericInteger(numeric, numericOK)
		case "!":
			allowed = primitiveIs(view, types.PrimitiveBool)
		default:
			allowed = true
		}
		if !allowed {
			a.addDiagnostic("semantic.generic.unary_constraint", "type parameter constraint does not permit this unary operation", expr.Span)
			return
		}
	}
}

func (a *analyzer) finalizeBinary(expr *ast.Expression, info ExprInfo) ExprInfo {
	if expr.Left == nil || expr.Right == nil {
		return info
	}
	left := a.info.Exprs[expr.Left.NodeID]
	right := a.info.Exprs[expr.Right.NodeID]
	if !left.Type.Valid() || !right.Type.Valid() {
		return info
	}
	operator := strings.TrimSpace(expr.Operator)
	comparison := isEqualityOperator(operator) || isOrderedOperator(operator)
	logical := isLogicalOperator(operator)
	shift := isShiftOperator(operator)
	leftNumeric, leftIsNumeric := a.info.Relations.View(left.Type).NumericInfo()
	rightNumeric, rightIsNumeric := a.info.Relations.View(right.Type).NumericInfo()
	typeParameterOperand := left.Type.Kind == types.TypeParameter || right.Type.Kind == types.TypeParameter

	leftTarget, rightTarget, result := left.Type, right.Type, left.Type
	switch {
	case left.Untyped && !right.Untyped:
		leftTarget, rightTarget, result = right.Type, right.Type, right.Type
	case !left.Untyped && right.Untyped:
		leftTarget, rightTarget, result = left.Type, left.Type, left.Type
	case left.Untyped && right.Untyped:
		result = promoteUntypedType(left.Type, leftNumeric, rightNumeric, leftIsNumeric, rightIsNumeric)
		leftTarget, rightTarget = result, result
	case a.info.Relations.Identical(left.Type, right.Type).OK:
		leftTarget, rightTarget, result = left.Type, left.Type, left.Type
	}
	if shift {
		leftTarget, result = left.Type, left.Type
		rightTarget = right.Type
	}
	if comparison {
		result = types.Builtin(types.PrimitiveBool)
	}
	if logical {
		result = leftTarget
	}
	info.Type = result
	info.LeftTarget = leftTarget
	info.RightTarget = rightTarget
	info.Mode = ExprValue
	info.Untyped = left.Untyped && right.Untyped
	if shift {
		// An untyped constant shifted by a non-constant value remains eligible
		// for conversion by its surrounding context.
		info.Untyped = left.Untyped
	}
	if left.Mode == ExprConstant && right.Mode == ExprConstant {
		info.Mode = ExprConstant
		info.Untyped = left.Untyped && right.Untyped
		if shift {
			info.Untyped = left.Untyped
		}
	}
	if typeParameterOperand {
		if !a.typeParameterBinaryAllowed(expr, left, right, operator) {
			if resolved, found := a.resolveOperator(expr.NodeID, operator, left, right, expr.Span); found {
				return resolved
			}
		}
		a.validateTypeParameterBinary(expr, left, right, operator)
		return info
	}
	if a.unresolvedDependencyTypeParameter(left.Type) || a.unresolvedDependencyTypeParameter(right.Type) {
		return info
	}
	if !a.nativeBinaryAllowed(expr, left, right, operator) {
		if resolved, found := a.resolveOperator(expr.NodeID, operator, left, right, expr.Span); found {
			return resolved
		}
	}

	integerOnly := operator == "%" || operator == "&" || operator == "|" || operator == "^" || operator == "&^" || shift
	if !shift {
		if left.Untyped && !right.Untyped {
			a.validateConstantTarget(*expr.Left, leftTarget, false)
		}
		if right.Untyped && !left.Untyped {
			a.validateConstantTarget(*expr.Right, rightTarget, false)
		}
		leftNumeric, leftIsNumeric = a.info.Relations.View(leftTarget).NumericInfo()
		rightNumeric, rightIsNumeric = a.info.Relations.View(rightTarget).NumericInfo()
	}
	numericOperator := isArithmeticOperator(operator) || comparison && leftIsNumeric && rightIsNumeric
	if numericOperator && (!leftIsNumeric || !rightIsNumeric) {
		leftString := isStringType(a.info.Relations.View(left.Type))
		rightString := isStringType(a.info.Relations.View(right.Type))
		if !(operator == "+" && leftString && rightString) && !(comparison && leftString && rightString) {
			a.addDiagnostic("hirgen.binary.numeric_operand", "numeric operator requires numeric operands", expr.Left.Span)
		}
		return info
	}
	if integerOnly && (!numericInteger(leftNumeric, leftIsNumeric) || !numericInteger(rightNumeric, rightIsNumeric)) {
		a.addDiagnostic("hirgen.binary.integer_operand", "remainder, bitwise, and shift operators require integer operands", expr.Left.Span)
		return info
	}
	if isOrderedOperator(operator) && (leftNumeric.Kind == types.NumericComplex || rightNumeric.Kind == types.NumericComplex) {
		a.addDiagnostic("hirgen.binary.ordered_complex", "ordered comparison requires integer, floating-point, or string operands", expr.Left.Span)
		return info
	}
	if !shift && leftIsNumeric && rightIsNumeric && !left.Untyped && !right.Untyped && !a.info.Relations.Identical(left.Type, right.Type).OK {
		a.addDiagnostic("hirgen.binary.numeric_types", "numeric operands must have identical types", expr.Left.Span)
		return info
	}
	if isEqualityOperator(operator) && !isNilExpression(*expr.Left) && !isNilExpression(*expr.Right) {
		if !a.info.Relations.Comparable(left.Type).OK || !a.info.Relations.Comparable(right.Type).OK {
			a.addDiagnostic("hirgen.binary.comparable", "equality operands must be comparable", expr.Span)
			return info
		}
		if !a.info.Relations.Identical(leftTarget, rightTarget).OK &&
			!a.info.Relations.Assignable(left.Type, right.Type).OK &&
			!a.info.Relations.Assignable(right.Type, left.Type).OK {
			a.addDiagnostic("hirgen.binary.comparable", "equality operands must be comparable", expr.Span)
		}
	}
	return info
}

func (a *analyzer) nativeBinaryAllowed(expr *ast.Expression, left, right ExprInfo, operator string) bool {
	leftView := a.info.Relations.View(left.Type)
	rightView := a.info.Relations.View(right.Type)
	leftNumeric, leftIsNumeric := leftView.NumericInfo()
	rightNumeric, rightIsNumeric := rightView.NumericInfo()
	compatible := left.Untyped || right.Untyped || a.info.Relations.Identical(left.Type, right.Type).OK
	switch {
	case isLogicalOperator(operator):
		return primitiveIs(leftView, types.PrimitiveBool) && primitiveIs(rightView, types.PrimitiveBool)
	case isEqualityOperator(operator):
		if expr != nil && expr.Left != nil && isNilExpression(*expr.Left) {
			return rightView.Nilable()
		}
		if expr != nil && expr.Right != nil && isNilExpression(*expr.Right) {
			return leftView.Nilable()
		}
		return leftView.StrictlyComparable() && rightView.StrictlyComparable() &&
			(compatible || a.info.Relations.Assignable(left.Type, right.Type).OK || a.info.Relations.Assignable(right.Type, left.Type).OK)
	case isOrderedOperator(operator):
		if isStringType(leftView) && isStringType(rightView) {
			return compatible
		}
		return leftIsNumeric && rightIsNumeric && compatible && leftNumeric.Kind != types.NumericComplex && rightNumeric.Kind != types.NumericComplex
	case isShiftOperator(operator):
		return numericInteger(leftNumeric, leftIsNumeric) && numericInteger(rightNumeric, rightIsNumeric)
	case isArithmeticOperator(operator):
		if operator == "+" && isStringType(leftView) && isStringType(rightView) {
			return compatible
		}
		if !leftIsNumeric || !rightIsNumeric || !compatible {
			return false
		}
		if operator == "%" || operator == "&" || operator == "|" || operator == "^" || operator == "&^" {
			return numericInteger(leftNumeric, true) && numericInteger(rightNumeric, true)
		}
		return true
	default:
		return true
	}
}

func (a *analyzer) typeParameterBinaryAllowed(expr *ast.Expression, left, right ExprInfo, operator string) bool {
	ref := left.Type
	if ref.Kind != types.TypeParameter {
		ref = right.Type
	}
	if left.Type.Kind == types.TypeParameter && right.Type.Kind == types.TypeParameter && !a.info.Relations.Identical(left.Type, right.Type).OK {
		return false
	}
	terms, ok := a.typeParameterTerms(ref)
	if !ok || len(terms) == 0 {
		if isEqualityOperator(operator) {
			constraint, exists := a.info.Relations.View(ref).Constraint()
			return exists && a.info.Relations.View(constraint).ComparableConstraint()
		}
		return false
	}
	for _, term := range terms {
		termLeft, termRight := left, right
		if termLeft.Type.Kind == types.TypeParameter {
			termLeft.Type = term
		}
		if termRight.Type.Kind == types.TypeParameter {
			termRight.Type = term
		}
		if !a.nativeBinaryAllowed(expr, termLeft, termRight, operator) {
			return false
		}
	}
	return true
}

func operatorMethod(operator string, unary bool) (string, bool) {
	if unary {
		switch operator {
		case "+":
			return "OpPos", true
		case "-":
			return "OpNeg", true
		case "!":
			return "OpNot", true
		case "^":
			return "OpBitNot", true
		}
		return "", false
	}
	switch operator {
	case "+":
		return "OpAdd", true
	case "-":
		return "OpSub", true
	case "*":
		return "OpMul", true
	case "/":
		return "OpDiv", true
	case "%":
		return "OpMod", true
	case "&":
		return "OpBitAnd", true
	case "|":
		return "OpBitOr", true
	case "^":
		return "OpBitXor", true
	case "&^":
		return "OpBitClear", true
	case "<<":
		return "OpLsh", true
	case ">>":
		return "OpRsh", true
	case "==":
		return "OpEq", true
	case "!=":
		return "OpNeq", true
	case "<":
		return "OpLt", true
	case "<=":
		return "OpLe", true
	case ">":
		return "OpGt", true
	case ">=":
		return "OpGe", true
	default:
		return "", false
	}
}

func (a *analyzer) resolveOperator(node ast.NodeID, operator string, left, right ExprInfo, span source.Span) (ExprInfo, bool) {
	unary := !right.Type.Valid()
	methodName, supported := operatorMethod(strings.TrimSpace(operator), unary)
	if !supported || !left.Type.Valid() {
		return ExprInfo{}, false
	}
	candidate, found := a.lookupSelector(left.Type, methodName, false)
	if !found || candidate.selection.Kind != SelectionMethod {
		return ExprInfo{}, false
	}
	selection := candidate.selection
	signature := selection.Signature
	wantParams := 1
	if unary {
		wantParams = 0
	}
	invalid := func(message string) (ExprInfo, bool) {
		a.addDiagnostic("semantic.operator.signature", methodName+" "+message, span)
		return ExprInfo{Mode: ExprValue}, true
	}
	if signature.Variadic {
		return invalid("must not be variadic")
	}
	if len(signature.Params) != wantParams {
		return invalid("has an invalid parameter count")
	}
	if len(signature.Results) != 1 || !signature.Results[0].Valid() || signature.Results[0].Kind == types.Void {
		return invalid("must return exactly one value")
	}
	if selection.Indirect && !left.Category.Addressable() {
		return invalid("requires an addressable receiver")
	}
	if !unary && !a.info.Relations.Assignable(right.Type, signature.Params[0].Type).OK {
		return invalid("cannot accept the right operand")
	}
	if (isEqualityOperator(operator) || isOrderedOperator(operator) || unary && operator == "!") &&
		!primitiveIs(a.info.Relations.View(signature.Results[0]), types.PrimitiveBool) {
		return invalid("must return Bool")
	}
	a.info.Operators[node] = OperatorSelection{Operator: operator, Selection: selection}
	return ExprInfo{Type: signature.Results[0], Mode: ExprValue, Results: []types.TypeRef{signature.Results[0]}}, true
}

func (a *analyzer) unresolvedDependencyTypeParameter(ref types.TypeRef) bool {
	if ref.Kind != types.Named || ref.Named.ModulePath == "" || ref.Named.DeclID == "" {
		return false
	}
	_, exported := a.dependency(ref.Named.ModulePath, string(ref.Named.DeclID))
	node, exists := a.info.TypeTable.Node(ref)
	return !exported && (!exists || !node.Underlying.Valid() && !node.AliasTarget.Valid())
}

func (a *analyzer) validateTypeParameterBinary(expr *ast.Expression, left, right ExprInfo, operator string) {
	if left.Type.Kind == types.TypeParameter && right.Type.Kind == types.TypeParameter &&
		!a.info.Relations.Identical(left.Type, right.Type).OK {
		a.addDiagnostic("semantic.generic.binary_types", "generic binary operands must have identical types", expr.Span)
		return
	}
	ref := left.Type
	if ref.Kind != types.TypeParameter {
		ref = right.Type
	}
	if !a.typeParameterInScope(ref, expr.NodeID) {
		return
	}
	terms, ok := a.typeParameterTerms(ref)
	constraint, hasConstraint := a.info.Relations.View(ref).Constraint()
	if isEqualityOperator(operator) && hasConstraint && a.info.Relations.View(constraint).ComparableConstraint() {
		return
	}
	if isEqualityOperator(operator) && (isNilExpression(*expr.Left) || isNilExpression(*expr.Right)) && ok {
		for _, term := range terms {
			if !a.info.Relations.View(term).Nilable() {
				a.addDiagnostic("semantic.generic.binary_constraint", "type parameter constraint does not permit comparison with nil", expr.Span)
				return
			}
		}
		return
	}
	if !ok {
		if a.deferTypeParameterCheck(ref) {
			return
		}
		a.addDiagnostic("semantic.generic.binary_constraint", "type parameter constraint does not permit this binary operation", expr.Span)
		return
	}
	integerOnly := operator == "%" || operator == "&" || operator == "|" || operator == "^" || operator == "&^" || isShiftOperator(operator)
	for _, term := range terms {
		view := a.info.Relations.View(term)
		numeric, numericOK := view.NumericInfo()
		allowed := false
		switch {
		case isLogicalOperator(operator):
			allowed = primitiveIs(view, types.PrimitiveBool)
		case isEqualityOperator(operator):
			allowed = view.StrictlyComparable()
		case isOrderedOperator(operator):
			allowed = view.Ordered()
		case integerOnly:
			allowed = numericInteger(numeric, numericOK)
		case operator == "+":
			allowed = numericOK || isStringType(view)
		case isArithmeticOperator(operator):
			allowed = numericOK
		}
		if !allowed {
			a.addDiagnostic("semantic.generic.binary_constraint", "type parameter constraint does not permit this binary operation", expr.Span)
			return
		}
	}
}

func (a *analyzer) typeParameterInScope(ref types.TypeRef, node ast.NodeID) bool {
	for scopeID := a.info.NodeScopes[node]; scopeID != 0; {
		scope := a.info.Scope(scopeID)
		if scope == nil {
			return false
		}
		for _, objectID := range scope.Objects {
			object, ok := a.info.Object(objectID)
			if ok && object.Kind == ObjectTypeParam && object.Type == ref {
				return true
			}
		}
		scopeID = scope.Parent
	}
	return false
}

func promoteUntypedType(left types.TypeRef, leftNumeric, rightNumeric types.NumericInfo, leftOK, rightOK bool) types.TypeRef {
	if !leftOK || !rightOK {
		return left
	}
	if leftNumeric.Kind == types.NumericComplex || rightNumeric.Kind == types.NumericComplex {
		return types.Builtin(types.PrimitiveComplex128)
	}
	if leftNumeric.Kind == types.NumericFloat || rightNumeric.Kind == types.NumericFloat {
		return types.Builtin(types.PrimitiveFloat64)
	}
	return left
}

func (a *analyzer) finalizeBuiltinCall(expr *ast.Expression, info ExprInfo, name string) ExprInfo {
	name = strings.TrimSpace(name)
	info.Mode = ExprValue
	switch name {
	case "len", "cap", "copy":
		info.Type = types.Builtin(types.PrimitiveInt)
	case "recover":
		info.Type = types.AnyType()
	case "complex":
		info.Type = types.Builtin(types.PrimitiveComplex128)
		if len(expr.Args) == 2 && primitiveIs(a.info.Relations.View(a.info.Exprs[expr.Args[0].NodeID].Type), types.PrimitiveFloat32) &&
			primitiveIs(a.info.Relations.View(a.info.Exprs[expr.Args[1].NodeID].Type), types.PrimitiveFloat32) {
			info.Type = types.Builtin(types.PrimitiveComplex64)
		}
	case "real", "imag":
		info.Type = types.Builtin(types.PrimitiveFloat64)
		if len(expr.Args) == 1 {
			argument := a.info.Exprs[expr.Args[0].NodeID].Type
			if argument.Kind == types.TypeParameter && a.typeParameterInScope(argument, expr.NodeID) {
				a.addDiagnostic("semantic.generic.complex_part", name+" does not accept a type parameter", expr.Args[0].Span)
			} else if primitiveIs(a.info.Relations.View(argument), types.PrimitiveComplex64) {
				info.Type = types.Builtin(types.PrimitiveFloat32)
			}
		}
	case "append":
		if len(expr.Args) != 0 {
			info.Type = a.info.Exprs[expr.Args[0].NodeID].Type
		}
	case "min", "max":
		info.Type = a.minMaxType(expr.Args)
	case "new":
		if len(expr.Args) == 1 {
			target := a.builtinArgumentType(expr.Args[0])
			if target.Valid() {
				info.Type = a.storeDerivedType(expr.NodeID, "new", types.Pointer, target)
			}
		}
	case "make":
		if len(expr.Args) != 0 {
			info.Type = a.builtinArgumentType(expr.Args[0])
		}
	case "delete", "clear", "close", "panic", "print", "println":
		info.Type = types.VoidType()
		info.Mode = ExprNoValue
	}
	if (name == "print" || name == "println") && expr.Ellipsis {
		a.addDiagnostic("semantic.builtin.print.ellipsis", name+" does not permit slice ellipsis", expr.Span)
	}
	if builtinConstant(name, expr.Args, a.info) {
		info.Mode = ExprConstant
		info.Untyped = true
		if name != "len" && name != "cap" {
			for i := range expr.Args {
				if !a.info.Exprs[expr.Args[i].NodeID].Untyped {
					info.Untyped = false
					break
				}
			}
		}
	}
	if info.Type.Valid() && info.Mode != ExprNoValue {
		info.Results = []types.TypeRef{info.Type}
	}
	return info
}

func (a *analyzer) builtinArgumentType(expr ast.Expression) types.TypeRef {
	if expr.Kind == ast.ExprIdent && expr.Name == "type" && expr.Type != nil && expr.Type.Kind != ast.TypeInvalid {
		return a.resolvedType(*expr.Type)
	}
	return a.info.Exprs[expr.NodeID].Type
}

func (a *analyzer) minMaxType(args []ast.Expression) types.TypeRef {
	var target types.TypeRef
	for i := range args {
		info := a.info.Exprs[args[i].NodeID]
		if !info.Type.Valid() {
			return types.TypeRef{}
		}
		if !info.Untyped {
			if !target.Valid() {
				target = info.Type
			} else if !a.info.Relations.Identical(target, info.Type).OK {
				return types.TypeRef{}
			}
		}
	}
	if target.Valid() {
		return target
	}
	if len(args) == 0 {
		return types.TypeRef{}
	}
	target = a.info.Exprs[args[0].NodeID].Type
	for i := 1; i < len(args); i++ {
		leftInfo, leftOK := a.info.Relations.View(target).NumericInfo()
		right := a.info.Exprs[args[i].NodeID].Type
		rightInfo, rightOK := a.info.Relations.View(right).NumericInfo()
		target = promoteUntypedType(target, leftInfo, rightInfo, leftOK, rightOK)
	}
	return target
}

func (a *analyzer) storeDerivedType(nodeID ast.NodeID, prefix string, kind types.Kind, elem types.TypeRef) types.TypeRef {
	id := types.TypeID(prefix + "." + strconv.FormatUint(uint64(nodeID), 10))
	node := types.TypeNode{ID: id, Kind: kind, Elem: elem}
	ref := types.TypeRef{Kind: kind, Node: id}
	if _, ok := a.info.TypeTable.Node(ref); ok {
		_ = a.info.TypeTable.Replace(node)
	} else {
		_ = a.info.TypeTable.Add(node)
	}
	return ref
}

func builtinConstant(name string, args []ast.Expression, info *ProgramInfo) bool {
	switch name {
	case "complex", "real", "imag", "min", "max":
		if len(args) == 0 {
			return false
		}
		for i := range args {
			if info.Exprs[args[i].NodeID].Mode != ExprConstant {
				return false
			}
		}
		return true
	case "len", "cap":
		if len(args) != 1 {
			return false
		}
		arg := info.Exprs[args[0].NodeID]
		view := info.Relations.View(arg.Type)
		return arg.Mode == ExprConstant && isStringType(view) || view.Shape() == types.Array
	default:
		return false
	}
}

func primitiveIs(view types.TypeView, want types.PrimitiveKind) bool {
	got, ok := view.Primitive()
	return ok && got == want
}

func isStringType(view types.TypeView) bool {
	return primitiveIs(view, types.PrimitiveString)
}

func numericInteger(info types.NumericInfo, ok bool) bool {
	return ok && (info.Kind == types.NumericSigned || info.Kind == types.NumericUnsigned)
}

func isNilExpression(expr ast.Expression) bool {
	return expr.Kind == ast.ExprLiteral && strings.TrimSpace(expr.Literal) == "nil"
}

func isLogicalOperator(operator string) bool {
	return operator == "&&" || operator == "||"
}

func isEqualityOperator(operator string) bool {
	return operator == "==" || operator == "!="
}

func isOrderedOperator(operator string) bool {
	switch operator {
	case "<", "<=", ">", ">=":
		return true
	default:
		return false
	}
}

func isShiftOperator(operator string) bool {
	switch operator {
	case "<<", ">>":
		return true
	default:
		return false
	}
}

func isArithmeticOperator(operator string) bool {
	switch operator {
	case "+", "-", "*", "/", "%", "&", "|", "^", "&^", "<<", ">>":
		return true
	default:
		return false
	}
}
