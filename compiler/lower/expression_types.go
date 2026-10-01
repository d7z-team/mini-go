package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) expressionType(expr ast.Expression, scope *funcScope) string {
	if typ, ok := l.semanticExpressionType(expr); ok {
		return typ
	}
	if expr.Kind == ast.ExprSelector {
		if signature, ok := l.semanticExpressionSignature(expr); ok {
			return l.signatureString(signature)
		}
		if selection, ok := l.semanticSelection(expr); ok && selection.Kind == check.SelectionPackageMember {
			if typ := l.formatSemanticType(selection.Type); typ != "" && typ != "Void" {
				return typ
			}
		}
	}
	if typ := l.expressionDeclaredType(expr, scope); typ != "" && typ != "Any" {
		return typ
	}
	switch expr.Kind {
	case ast.ExprEmbed:
		if info, ok := l.semantic.Embeds[expr.NodeID]; ok {
			return l.formatSemanticType(info.Type)
		}
	case ast.ExprCall:
		if target, ok := l.typeConversionCallTarget(expr, scope); ok {
			return target
		}
		if expr.Callee != nil && expr.Callee.Kind == ast.ExprSelector {
			if _, ok := l.selectorTypeExport(*expr.Callee, scope); ok {
				if target := l.selectorConversionType(*expr.Callee, scope); target != "" {
					return target
				}
			}
		}
		if expr.Callee != nil && expr.Callee.Kind == ast.ExprIdent && l.isBuiltinCallName(expr.Callee.Name, expr.Callee.Span, scope) {
			return l.builtinCallType(expr, scope)
		}
		if expr.Callee != nil {
			if results, ok := l.callResultTypes(*expr.Callee, scope); ok && len(results) == 1 {
				return results[0]
			}
		}
	case ast.ExprLiteral:
		return literalType(expr)
	case ast.ExprBinary:
		if expr.Operator == "==" || expr.Operator == "!=" || expr.Operator == "<" || expr.Operator == "<=" || expr.Operator == ">" || expr.Operator == ">=" || expr.Operator == "&&" || expr.Operator == "||" {
			return "Bool"
		}
		return l.binaryExpressionType(expr, scope)
	case ast.ExprUnary:
		if expr.Operator == "!" {
			return "Bool"
		}
		if expr.Operand != nil {
			return l.expressionType(*expr.Operand, scope)
		}
	case ast.ExprConvert, ast.ExprAssert:
		return l.resolveSourceTypePtr(expr.Type, nil)
	case ast.ExprAddr:
		if expr.Operand != nil {
			if typ := l.expressionType(*expr.Operand, scope); typ != "" {
				return "Ptr<" + typ + ">"
			}
		}
	case ast.ExprDeref:
		if expr.Operand != nil {
			elem, _ := l.pointerElementType(l.expressionType(*expr.Operand, scope))
			return elem
		}
	case ast.ExprReceive:
		if expr.Operand != nil {
			return l.channelElementType(l.expressionType(*expr.Operand, scope))
		}
	case ast.ExprIndex:
		if expr.Operand != nil {
			return l.indexElementType(l.expressionType(*expr.Operand, scope))
		}
	case ast.ExprSlice:
		if expr.Operand != nil {
			return l.sliceResultType(l.expressionType(*expr.Operand, scope))
		}
	case ast.ExprSelector:
		if export, ok := l.selectorExport(expr, scope); ok {
			return l.resolveType(export.Type)
		}
		if expr.Operand != nil {
			return l.memberType(l.expressionType(*expr.Operand, scope), expr.Field)
		}
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if scope != nil {
			if _, typ, ok := l.lookupLocalOrConst(name, scope); ok && typ != "" {
				return typ
			}
			if _, typ, ok := l.lookupUpvalue(name, scope); ok && typ != "" {
				return typ
			}
			if _, ok := l.resolveUpvalue(name, scope); ok {
				if _, typ, ok := l.lookupUpvalue(name, scope); ok && typ != "" {
					return typ
				}
			}
		}
		if _, typ, ok := l.lookupConstID(name, scope); ok && typ != "" {
			return typ
		}
		if _, ok := l.functions[name]; ok {
			if signature, found := l.semanticFunctionSignature(name); found {
				return l.signatureString(signature)
			}
		}
		if export, ok := l.dotImportExport(name, expr.Span); ok {
			return l.resolveType(export.Type)
		}
		return l.typeRefString(l.globalTypes[name])
	case ast.ExprComposite:
		return l.expressionDeclaredType(expr, scope)
	case ast.ExprFunc:
		return l.sourceFunctionTypeString(expr.Func.Params, expr.Func.Results)
	}
	return ""
}

func (l *lowerer) defaultedExpressionType(expr ast.Expression, scope *funcScope) string {
	typ := strings.TrimSpace(l.expressionType(expr, scope))
	if typ == "" || !l.untypedConstExpression(expr, scope) {
		return typ
	}
	raw, sourceType, ok := l.constValue(expr, scope)
	if !ok {
		sourceType = typ
	}
	return l.resolveType(defaultUntypedConstType(sourceType, raw))
}

func (l *lowerer) expressionDeclaredType(expr ast.Expression, scope *funcScope) string {
	if typ, ok := l.inferredArrayCompositeType(expr, scope); ok {
		return typ
	}
	return l.resolveSourceTypePtr(expr.Type, scope)
}

func (l *lowerer) inferredArrayCompositeType(expr ast.Expression, scope *funcScope) (string, bool) {
	if expr.Kind != ast.ExprComposite || expr.Type == nil || expr.Type.Kind != ast.TypeArray || !expr.Type.LenInfer || expr.Type.Elem == nil {
		return "", false
	}
	elemType := l.resolveSourceTypeInScope(*expr.Type.Elem, scope)
	if elemType == "" {
		elemType = "Any"
	}
	maxIndex := int64(-1)
	nextIndex := int64(0)
	for _, item := range expr.Items {
		index := nextIndex
		if item.Key != nil {
			key, ok := l.arrayCompositeKeyIndex(*item.Key, scope)
			if !ok {
				return "", false
			}
			index = key
		}
		if index > maxIndex {
			maxIndex = index
		}
		nextIndex = index + 1
	}
	length := maxIndex + 1
	if length < 0 {
		length = 0
	}
	return arrayTypeString(length, elemType), true
}

func (l *lowerer) builtinCallType(expr ast.Expression, scope *funcScope) string {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
		return ""
	}
	switch strings.TrimSpace(expr.Callee.Name) {
	case "len", "cap", "copy":
		return "Int"
	case "recover":
		return "Any"
	case "complex":
		if len(expr.Args) != 2 {
			return ""
		}
		leftType := l.underlyingConstType(l.expressionType(expr.Args[0], scope))
		rightType := l.underlyingConstType(l.expressionType(expr.Args[1], scope))
		if leftType == "Float32" && rightType == "Float32" {
			return "Complex64"
		}
		return "Complex128"
	case "real", "imag":
		if len(expr.Args) != 1 {
			return ""
		}
		if l.underlyingConstType(l.expressionType(expr.Args[0], scope)) == "Complex64" {
			return "Float32"
		}
		return "Float64"
	case "append":
		if len(expr.Args) == 0 {
			return ""
		}
		return l.expressionType(expr.Args[0], scope)
	case "min", "max":
		typ, ok := l.minMaxTargetType(expr, scope)
		if ok {
			return typ
		}
	case "new":
		if len(expr.Args) == 1 {
			if isTypeArgumentExpression(expr.Args[0]) {
				typ := l.resolveSourceTypePtr(expr.Args[0].Type, nil)
				return "Ptr<" + typ + ">"
			}
			if typ, ok := l.resolveNamedTypeArgument(expr.Args[0], scope); ok {
				return "Ptr<" + typ + ">"
			}
			if typ := l.defaultedExpressionType(expr.Args[0], scope); typ != "" {
				return "Ptr<" + typ + ">"
			}
		}
	case "make":
		if len(expr.Args) >= 1 {
			if typ := l.resolveSourceTypePtr(expr.Args[0].Type, nil); typ != "" {
				return typ
			}
			return l.expressionType(expr.Args[0], scope)
		}
	}
	return ""
}

func (l *lowerer) localDeclType(decl ast.ValueDecl, index int, scope *funcScope) string {
	if typ := l.resolveSourceType(decl.Type); typ != "" {
		return typ
	}
	if len(decl.Values) == 1 && len(decl.Names) > 1 {
		if typ := l.multiResultValueType(decl.Values[0], index, scope); typ != "" {
			return typ
		}
	}
	if index >= 0 && index < len(decl.Values) && len(decl.Values) == len(decl.Names) {
		if typ := l.defaultedExpressionType(decl.Values[index], scope); typ != "" {
			return typ
		}
	}
	if len(decl.Names) == 1 && len(decl.Values) == 1 {
		if typ := l.defaultedExpressionType(decl.Values[0], scope); typ != "" {
			return typ
		}
	}
	return "Any"
}

func (l *lowerer) multiResultValueType(expr ast.Expression, index int, scope *funcScope) string {
	if index < 0 {
		return ""
	}
	switch expr.Kind {
	case ast.ExprReceive, ast.ExprAssert:
		if index == 0 {
			return l.expressionType(expr, scope)
		}
		if index == 1 {
			return "Bool"
		}
	case ast.ExprIndex:
		if index == 0 {
			return l.expressionType(expr, scope)
		}
		if index == 1 {
			if _, _, ok := l.mapKeyValueTypes(l.expressionType(derefExpr(expr.Operand), scope)); ok {
				return "Bool"
			}
		}
	case ast.ExprCall:
		if results, ok := l.semanticExpressionResults(expr); ok {
			if index < len(results) {
				return results[index]
			}
			return ""
		}
		if expr.Callee == nil {
			return ""
		}
		if results, ok := l.callResultTypes(*expr.Callee, scope); ok {
			if index < len(results) {
				return results[index]
			}
			return ""
		}
		if index == 0 {
			if typ := l.expressionType(expr, scope); typ != "" {
				return typ
			}
		}
	}
	return ""
}

func (l *lowerer) resultTypes(fields []ast.Field) []string {
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, l.resolveSourceType(field.Type))
	}
	return out
}

func (l *lowerer) paramTypes(fields []ast.Field) ([]string, bool) {
	out := make([]string, 0, len(fields))
	var variadic bool
	for _, field := range fields {
		out = append(out, l.fieldTypeString(field))
		if field.Variadic {
			variadic = true
		}
	}
	return out, variadic
}

func literalType(expr ast.Expression) string {
	if expr.Type != nil {
		if typ := typeString(*expr.Type); typ != "" && typ != "Any" {
			return typ
		}
	}
	text := strings.TrimSpace(expr.Literal)
	if text == "true" || text == "false" {
		return "Bool"
	}
	if strings.HasPrefix(text, "\"") {
		return "String"
	}
	if strings.ContainsAny(text, ".eE") {
		return "Float64"
	}
	if text != "" && text != "nil" {
		return "Int"
	}
	return ""
}

func (l *lowerer) indexElementType(typ string) string {
	typ = strings.TrimSpace(typ)
	resolved := l.resolveNamedUnderlyingType(typ)
	if resolved == "String" {
		return "Uint8"
	}
	if view, ok := l.typeView(resolved); ok {
		switch view.Shape() {
		case types.Slice:
			elem, ok := view.Elem()
			if ok {
				return l.typeRefString(elem)
			}
		case types.Array:
			_, elem, ok := view.Array()
			if ok {
				return l.typeRefString(elem)
			}
		case types.Pointer:
			if arrayType, ok := l.pointerArrayType(resolved); ok {
				_, elem, ok := l.arrayTypeInfo(arrayType)
				if ok {
					return elem
				}
			}
		case types.Map:
			_, valueType, ok := l.mapTypeInfo(resolved)
			if ok {
				return valueType
			}
		}
	}
	if decl, ok := l.typeDeclFor(resolved); ok {
		switch decl.Kind {
		case ast.TypeArray, ast.TypeSlice:
			if decl.Elem != nil {
				return l.resolveSourceType(*decl.Elem)
			}
		case ast.TypeMap:
			if _, valueType, ok := l.mapKeyValueTypes(resolved); ok {
				return valueType
			}
		}
	}
	return ""
}

func (l *lowerer) mapIndexKeyType(object ast.Expression, scope *funcScope) string {
	keyType, _, _ := l.mapKeyValueTypes(l.expressionType(object, scope))
	return keyType
}

func (l *lowerer) mapKeyValueTypes(typ string) (string, string, bool) {
	typ = strings.TrimSpace(typ)
	resolved := l.resolveType(typ)
	if keyType, valueType, ok := l.mapTypeInfo(resolved); ok {
		return keyType, valueType, true
	}
	if decl, ok := l.typeDeclFor(resolved); ok && decl.Kind == ast.TypeMap && decl.Key != nil && decl.Elem != nil {
		keyType := l.resolveSourceType(*decl.Key)
		valueType := l.resolveSourceType(*decl.Elem)
		if keyType != "" && valueType != "" {
			return keyType, valueType, true
		}
	}
	return "", "", false
}

func (l *lowerer) validateMapKeyComparable(typ string, span source.Span) bool {
	keyType, _, ok := l.mapKeyValueTypes(typ)
	if !ok {
		return true
	}
	if l.isComparableType(keyType) {
		return true
	}
	l.add("hirgen.map.key.comparable", "map key type must be comparable", span)
	return false
}
