package semantic

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/types"
)

type constantBinding struct {
	expr  ast.Expression
	typ   ast.TypeExpr
	scope ScopeID
}

const (
	constantEvaluating uint8 = iota + 1
	constantDone
	constantFailed
)

func (a *analyzer) indexPackageConstants(program ast.Program) {
	for i := range program.Files {
		file := &program.Files[i]
		scope := a.files[file.Path]
		for j := range file.Decls {
			decl := &file.Decls[j]
			if decl.Kind != ast.DeclConst {
				continue
			}
			a.bindConstantDecl(*decl.Const, scope)
		}
	}
}

func (a *analyzer) bindAnalyzedConstants(decl ast.ValueDecl, scope ScopeID) {
	a.bindConstantDecl(decl, scope)
	for _, name := range decl.Names {
		if object, ok := a.info.Lookup(scope, name); ok && object.Kind == ObjectConst {
			value, valid := a.evaluateConstantObject(object.ID)
			if !valid {
				continue
			}
			object.Untyped = value.Untyped
			if ref, err := a.parser.Parse(value.Type); err == nil {
				object.Type = ref
			}
			a.info.Objects[object.ID] = object
		}
	}
}

func (a *analyzer) bindConstantDecl(decl ast.ValueDecl, scope ScopeID) {
	for i, name := range decl.Names {
		object, ok := a.info.Lookup(scope, strings.TrimSpace(name))
		if !ok || object.Kind != ObjectConst || object.Blank || i >= len(decl.Values) {
			continue
		}
		a.constantBindings[object.ID] = constantBinding{expr: decl.Values[i], typ: decl.Type, scope: scope}
	}
}

func (a *analyzer) evaluateConstantObject(id ObjectID) (constant.Value, bool) {
	if value, ok := a.info.ConstObjects[id]; ok {
		return value, true
	}
	if a.constantStates[id] == constantEvaluating {
		if binding, ok := a.constantBindings[id]; ok {
			a.addDiagnostic("semantic.const.cycle", "constant declaration cycle", binding.expr.Span)
		}
		a.constantStates[id] = constantFailed
		return constant.Value{}, false
	}
	if a.constantStates[id] == constantDone || a.constantStates[id] == constantFailed {
		return constant.Value{}, false
	}
	binding, ok := a.constantBindings[id]
	if !ok {
		object, exists := a.info.Object(id)
		if !exists || object.ModulePath == "" || object.ExportName == "" {
			return constant.Value{}, false
		}
		export, exists := a.dependency(object.ModulePath, object.ExportName)
		if !exists {
			return constant.Value{}, false
		}
		value, valid := a.importedConstant(export)
		if valid {
			a.info.ConstObjects[id] = value
		}
		return value, valid
	}
	a.constantStates[id] = constantEvaluating
	value, ok := a.evaluateConstantExpression(binding.expr, binding.scope)
	if !ok {
		a.constantStates[id] = constantFailed
		return constant.Value{}, false
	}
	if !value.Untyped {
		if target, err := a.parser.Parse(value.Type); err == nil && !a.constantValueFits(value, target, true) {
			a.addDiagnostic("semantic.const.representable", "constant is not representable by "+value.Type, binding.expr.Span)
			a.constantStates[id] = constantFailed
			return constant.Value{}, false
		}
	}
	if declared := a.resolvedType(binding.typ); declared.Valid() {
		if !a.constantValueFits(value, declared, false) {
			a.addDiagnostic("semantic.const.representable", "constant is not representable by "+types.FormatWithTable(a.info.TypeTable, declared), binding.expr.Span)
			a.constantStates[id] = constantFailed
			return constant.Value{}, false
		}
		value.Type = types.FormatWithTable(a.info.TypeTable, declared)
		value.Untyped = false
		value = a.roundTypedConstant(value, declared)
	}
	a.constantStates[id] = constantDone
	a.info.ConstObjects[id] = value
	return value, true
}

func (a *analyzer) evaluateConstantExpression(expr ast.Expression, scope ScopeID) (constant.Value, bool) {
	if value, ok := a.info.Constants[expr.NodeID]; ok {
		return value, true
	}
	var value constant.Value
	var ok bool
	switch expr.Kind {
	case ast.ExprLiteral:
		literal := strings.TrimSpace(expr.Literal)
		if literal == "true" || literal == "false" {
			value, ok = constant.Value{Text: literal, Type: "Bool", Untyped: true}, true
		} else if strings.HasSuffix(literal, "i") {
			imaginaryPart, valid := constant.Numeric(strings.TrimSuffix(literal, "i"), "Float64", true)
			if valid {
				value, ok = constant.Complex(constant.Value{Text: "0"}, imaginaryPart, "Complex128", true)
			}
		} else if len(literal) >= 2 && literal[0] == '\'' {
			text, err := strconv.Unquote(literal)
			if err == nil && len([]rune(text)) == 1 {
				value, ok = constant.Integer(strconv.FormatInt(int64([]rune(text)[0]), 10), "Int32", true)
			}
		} else if len(literal) >= 2 && (literal[0] == '"' || literal[0] == '`') {
			text, err := strconv.Unquote(literal)
			if err == nil {
				value, ok = constant.String(text, "String", true), true
			}
		} else {
			value, ok = constant.ParseIntegerLiteral(expr.Literal)
		}
		if !ok {
			value, ok = constant.Numeric(expr.Literal, "Float64", true)
		}
		if ok {
			if ref := a.resolvedTypePtr(expr.Type); ref.Valid() {
				value.Type = types.FormatWithTable(a.info.TypeTable, ref)
			}
		}
	case ast.ExprIdent:
		if expr.Name == "true" || expr.Name == "false" {
			value, ok = constant.Value{Text: expr.Name, Type: "Bool", Untyped: true}, true
			break
		}
		object, found := a.info.Object(a.info.Uses[expr.NodeID])
		if !found {
			object, found = a.info.Lookup(scope, expr.Name)
		}
		if found && object.Kind == ObjectConst {
			value, ok = a.evaluateConstantObject(object.ID)
		}
	case ast.ExprSelector:
		if expr.Operand != nil && expr.Operand.Kind == ast.ExprIdent {
			importObject, found := a.info.Lookup(scope, expr.Operand.Name)
			if found && importObject.Kind == ObjectImport {
				if export, exists := a.dependency(importObject.ImportPath, expr.Field); exists && export.Kind == ObjectConst {
					value, ok = a.importedConstant(export)
				}
			}
		}
	case ast.ExprUnary:
		if expr.Operand != nil {
			operand, valid := a.evaluateConstantExpression(*expr.Operand, scope)
			if valid {
				value, ok = constant.Unary(expr.Operator, operand)
				if ok && expr.Operator == "^" && !operand.Untyped {
					ref, err := a.parser.Parse(operand.Type)
					if numeric, numericOK := a.info.Relations.View(ref).NumericInfo(); err == nil && numericOK && numeric.Kind == types.NumericUnsigned {
						value.Text, ok = constant.AddSignedDecimal(value.Text, constant.Pow2UnsignedDecimal(numeric.Bits))
					}
				}
			}
		}
	case ast.ExprBinary:
		if expr.Left != nil && expr.Right != nil {
			left, leftOK := a.evaluateConstantExpression(*expr.Left, scope)
			right, rightOK := a.evaluateConstantExpression(*expr.Right, scope)
			if leftOK && rightOK {
				if expr.Operator != "<<" && expr.Operator != ">>" && left.Untyped != right.Untyped {
					typed, untyped := left, &right
					if left.Untyped {
						typed, untyped = right, &left
					}
					if target, err := a.parser.Parse(typed.Type); err == nil {
						if converted, valid := a.convertConstant(*untyped, target); valid {
							*untyped = a.roundTypedConstant(converted, target)
						}
					}
				}
				value, ok = constant.Binary(expr.Operator, left, right)
			}
		}
	case ast.ExprConvert:
		if expr.Operand != nil {
			value, ok = a.evaluateConstantExpression(*expr.Operand, scope)
			if ok {
				value, ok = a.convertConstant(value, a.resolvedTypePtr(expr.Type))
			}
		}
	case ast.ExprCall:
		value, ok = a.evaluateConstantCall(expr, scope)
	}
	if ok {
		if !value.Untyped {
			if target, err := a.parser.Parse(value.Type); err == nil {
				value = a.roundTypedConstant(value, target)
			}
		}
		a.info.Constants[expr.NodeID] = value
	}
	return value, ok
}

func (a *analyzer) convertConstant(value constant.Value, target types.TypeRef) (constant.Value, bool) {
	if _, primitive := a.info.Relations.View(target).Primitive(); !primitive {
		return constant.Value{}, false
	}
	if numeric, ok := a.info.Relations.View(target).NumericInfo(); ok {
		if numeric.Kind == types.NumericComplex && value.Imag == "" {
			value.Real, value.Imag, value.Text = value.Text, "0", ""
		}
		if numeric.Kind != types.NumericComplex && value.Imag != "" {
			imaginaryPart, ok := constant.ParseRationalLiteral(value.Imag)
			if !ok || imaginaryPart.Numerator != "0" {
				return constant.Value{}, false
			}
			value.Text, value.Real, value.Imag = value.Real, "", ""
		}
	}
	if primitive, _ := a.info.Relations.View(target).Primitive(); primitive == types.PrimitiveString {
		if _, err := strconv.Unquote(value.Text); err != nil {
			n, ok := value.Int64()
			if !ok {
				return constant.Value{}, false
			}
			if n < 0 || n > 0x10ffff || n >= 0xd800 && n <= 0xdfff {
				n = 0xfffd
			}
			value = constant.String(string(rune(n)), "String", false)
		}
	}
	value.Type = types.FormatWithTable(a.info.TypeTable, target)
	value.Untyped = false
	return value, true
}

func (a *analyzer) roundTypedConstant(value constant.Value, target types.TypeRef) constant.Value {
	numeric, ok := a.info.Relations.View(target).NumericInfo()
	if !ok || numeric.Kind != types.NumericFloat && numeric.Kind != types.NumericComplex {
		return value
	}
	if !a.constantValueFits(value, target, true) {
		return value
	}
	if numeric.Kind == types.NumericFloat {
		if rounded, ok := value.RoundFloat(numeric.Bits); ok {
			value = rounded
		}
	} else {
		if value.Imag == "" {
			value.Real, value.Imag, value.Text = value.Text, "0", ""
		}
		for _, part := range []*string{&value.Real, &value.Imag} {
			exact, valid := constant.ParseRationalLiteral(*part)
			if rounded, ok := constant.RoundRationalFloat(exact, numeric.Bits/2); valid && ok {
				*part = rounded.String()
			}
		}
	}
	return value
}

func (a *analyzer) importedConstant(export DependencyExport) (constant.Value, bool) {
	typ := export.Type
	if ref, err := a.parser.Parse(typ); err == nil {
		if primitive, _ := a.info.Relations.View(ref).Primitive(); primitive == types.PrimitiveString || primitive == types.PrimitiveBool || primitive == types.PrimitiveComplex64 || primitive == types.PrimitiveComplex128 {
			typ = types.FormatWithTable(a.info.TypeTable, types.Builtin(primitive))
		}
	}
	value, ok := constant.FromJSON(export.Value, typ, export.Untyped)
	value.Type = export.Type
	return value, ok
}

func (a *analyzer) evaluateConstantCall(expr ast.Expression, scope ScopeID) (constant.Value, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
		return constant.Value{}, false
	}
	name := strings.TrimSpace(expr.Callee.Name)
	if name == "complex" && len(expr.Args) == 2 {
		realPart, rok := a.evaluateConstantExpression(expr.Args[0], scope)
		imaginaryPart, iok := a.evaluateConstantExpression(expr.Args[1], scope)
		if !rok || !iok {
			return constant.Value{}, false
		}
		typ := "Complex128"
		if realPart.Type == "Float32" || imaginaryPart.Type == "Float32" {
			typ = "Complex64"
		}
		return constant.Complex(realPart, imaginaryPart, typ, realPart.Untyped && imaginaryPart.Untyped)
	}
	if (name == "real" || name == "imag") && len(expr.Args) == 1 {
		value, ok := a.evaluateConstantExpression(expr.Args[0], scope)
		if !ok || value.Imag == "" {
			return constant.Value{}, false
		}
		text := value.Real
		if name == "imag" {
			text = value.Imag
		}
		typ := "Float64"
		if value.Type == "Complex64" {
			typ = "Float32"
		}
		return constant.Numeric(text, typ, value.Untyped)
	}
	if len(expr.Args) == 1 {
		if object, ok := a.info.Lookup(scope, name); ok && (object.Kind == ObjectType || object.Kind == ObjectTypeParam) {
			value, valid := a.evaluateConstantExpression(expr.Args[0], scope)
			if !valid {
				return constant.Value{}, false
			}
			return a.convertConstant(value, object.Type)
		}
		if name == "len" {
			if value, ok := a.evaluateConstantExpression(expr.Args[0], scope); ok {
				if text, err := strconv.Unquote(value.Text); err == nil {
					return constant.Integer(strconv.Itoa(len(text)), "Int", true)
				}
			}
		}
		if name == "len" || name == "cap" {
			arg := expr.Args[0]
			if !ContainsNonConstantLenOperand(arg) {
				if length, ok := a.constantArrayOperandLength(arg, scope); ok {
					return constant.Integer(strconv.FormatInt(length, 10), "Int", true)
				}
				if ref := a.info.Exprs[arg.NodeID].Type; ref.Valid() {
					view := a.info.Relations.View(ref)
					if view.Shape() == types.Pointer {
						if elem, pointer := view.Elem(); pointer {
							view = a.info.Relations.View(elem)
						}
					}
					if length, _, array := view.Array(); array && length >= 0 {
						return constant.Integer(strconv.FormatInt(length, 10), "Int", true)
					}
				}
			}
		}
	}
	if (name == "min" || name == "max") && len(expr.Args) != 0 {
		value, ok := a.evaluateConstantExpression(expr.Args[0], scope)
		if !ok {
			return constant.Value{}, false
		}
		for i := 1; i < len(expr.Args); i++ {
			next, valid := a.evaluateConstantExpression(expr.Args[i], scope)
			if !valid {
				return constant.Value{}, false
			}
			var comparison int
			if left, err := strconv.Unquote(value.Text); err == nil {
				right, err := strconv.Unquote(next.Text)
				if err != nil {
					return constant.Value{}, false
				}
				comparison = strings.Compare(left, right)
			} else {
				left, leftOK := value.Rational()
				right, rightOK := next.Rational()
				if !leftOK || !rightOK {
					return constant.Value{}, false
				}
				comparison = constant.CompareRational(left, right)
			}
			if name == "min" && comparison > 0 || name == "max" && comparison < 0 {
				value = next
			}
		}
		return value, true
	}
	return constant.Value{}, false
}

func (a *analyzer) constantArrayOperandLength(expr ast.Expression, scope ScopeID) (int64, bool) {
	for expr.Kind == ast.ExprAddr && expr.Operand != nil {
		expr = *expr.Operand
	}
	if expr.Kind != ast.ExprComposite || expr.Type == nil || expr.Type.Kind != ast.TypeArray {
		return 0, false
	}
	if !expr.Type.LenInfer {
		if expr.Type.Len == nil {
			return 0, false
		}
		value, ok := a.evaluateConstantExpression(*expr.Type.Len, scope)
		if !ok {
			return 0, false
		}
		length, ok := value.Int64()
		return length, ok && length >= 0
	}
	maxIndex, nextIndex := int64(-1), int64(0)
	for _, item := range expr.Items {
		index := nextIndex
		if item.Key != nil {
			value, ok := a.evaluateConstantExpression(*item.Key, scope)
			if !ok {
				return 0, false
			}
			index, ok = value.Int64()
			if !ok || index < 0 {
				return 0, false
			}
		}
		if index > maxIndex {
			maxIndex = index
		}
		nextIndex = index + 1
	}
	return maxIndex + 1, true
}

// ContainsNonConstantLenOperand reports calls or receives that require evaluating
// an array operand of len/cap or an index-only range.
func ContainsNonConstantLenOperand(expr ast.Expression) bool {
	if expr.Kind == ast.ExprCall || expr.Kind == ast.ExprReceive {
		return true
	}
	children := []*ast.Expression{expr.Left, expr.Right, expr.Operand, expr.Callee, expr.Index, expr.Start, expr.End, expr.Max}
	for _, child := range children {
		if child != nil && ContainsNonConstantLenOperand(*child) {
			return true
		}
	}
	for i := range expr.Args {
		if ContainsNonConstantLenOperand(expr.Args[i]) {
			return true
		}
	}
	for i := range expr.Items {
		if expr.Items[i].Key != nil && ContainsNonConstantLenOperand(*expr.Items[i].Key) || ContainsNonConstantLenOperand(expr.Items[i].Value) {
			return true
		}
	}
	return false
}

func (a *analyzer) arrayLength(expr ast.Expression, scope ScopeID) (int64, bool) {
	value, ok := a.evaluateConstantExpression(expr, scope)
	if !ok {
		info := a.info.Exprs[expr.NodeID]
		code, message := "semantic.array.length.integer", "array length must be an integer constant"
		nonConstant := info.Mode != ExprConstant
		if expr.Kind == ast.ExprIdent {
			if binding, exists := a.constantBindings[a.info.Uses[expr.NodeID]]; exists {
				nonConstant = ContainsNonConstantLenOperand(binding.expr)
			}
		}
		if nonConstant {
			code, message = "semantic.array.length.constant", "array length must be constant"
		}
		a.addDiagnostic(code, message, expr.Span)
		return types.UnknownArrayLength, false
	}
	if rational, parsed := value.Rational(); parsed {
		if _, integer := rational.Integer(); !integer {
			a.addDiagnostic("semantic.array.length.integer", "array length must be an integer constant", expr.Span)
			return types.UnknownArrayLength, false
		}
	}
	length, ok := value.Int64()
	if !ok {
		a.addDiagnostic("semantic.array.length.overflow", "array length must be representable by Int", expr.Span)
		return types.UnknownArrayLength, false
	}
	if length < 0 {
		a.addDiagnostic("semantic.array.length.negative", "array length must be non-negative", expr.Span)
		return types.UnknownArrayLength, false
	}
	a.info.ArrayLengths[expr.NodeID] = length
	return length, true
}
