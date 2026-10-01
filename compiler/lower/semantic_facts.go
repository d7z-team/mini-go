package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) semanticFunctionSignature(name string) (types.FunctionSignature, bool) {
	if l.semantic == nil {
		return types.FunctionSignature{}, false
	}
	object, ok := l.semantic.Lookup(l.semantic.PackageScope, strings.TrimSpace(name))
	if !ok {
		return types.FunctionSignature{}, false
	}
	signature, ok := l.semantic.Relations.View(object.Type).Function()
	if !ok {
		return types.FunctionSignature{}, false
	}
	return signature, true
}

func (l *lowerer) semanticCallSignature(expr ast.Expression) (types.FunctionSignature, bool) {
	if l.semantic == nil || expr.Kind != ast.ExprCall {
		return types.FunctionSignature{}, false
	}
	call, ok := l.semantic.Calls[expr.NodeID]
	if !ok {
		return types.FunctionSignature{}, false
	}
	return call.Signature, true
}

func (l *lowerer) semanticSelection(expr ast.Expression) (check.Selection, bool) {
	if l.semantic == nil || expr.Kind != ast.ExprSelector {
		return check.Selection{}, false
	}
	selection, ok := l.semantic.Selections[expr.NodeID]
	return selection, ok && selection.Kind != check.SelectionInvalid
}

func (l *lowerer) semanticOperator(node ast.NodeID) (check.OperatorSelection, bool) {
	if l.semantic == nil || node == 0 {
		return check.OperatorSelection{}, false
	}
	selection, ok := l.semantic.Operators[node]
	return selection, ok && selection.Selection.Kind == check.SelectionMethod
}

func (l *lowerer) semanticMethodInfo(expr ast.Expression) (methodInfo, bool, bool) {
	selection, ok := l.semanticSelection(expr)
	if !ok || selection.Kind != check.SelectionMethod && selection.Kind != check.SelectionMethodExpression {
		return methodInfo{}, false, false
	}
	method, found := l.methodInfoFromSelection(selection)
	return method, selection.Interface, found
}

func (l *lowerer) methodInfoFromSelection(selection check.Selection) (methodInfo, bool) {
	signature := selection.Signature
	if selection.Kind == check.SelectionMethodExpression {
		if len(signature.Params) == 0 {
			return methodInfo{}, false
		}
		signature.Params = signature.Params[1:]
	}
	receiver := l.formatSemanticType(selection.DeclaringReceiver)
	if receiver == "" {
		return methodInfo{}, false
	}
	modulePath := strings.TrimSpace(selection.ModulePath)
	function := strings.TrimSpace(selection.FunctionID)
	if modulePath == strings.TrimSpace(l.modulePath) {
		modulePath = ""
		function = methodID(l.localMethodReceiverType(receiver), selection.Name)
	} else if modulePath != "" {
		l.ensureSourceRequirement(modulePath)
		if function == "" {
			base := receiver
			if strings.HasPrefix(base, "Ptr<") && strings.HasSuffix(base, ">") {
				base = base[len("Ptr<") : len(base)-1]
			}
			if imported, ok := l.importedTypeInfo(base); ok {
				for _, method := range imported.Methods {
					if method.Name == selection.Name {
						function = method.FunctionID
						break
					}
				}
			}
		}
	}
	if function == "" {
		function = methodID(receiver, selection.Name)
	}
	return methodInfo{
		ModulePath: modulePath, FunctionID: function,
		Receiver: selection.DeclaringReceiver, Signature: signature,
	}, true
}

func (l *lowerer) semanticInterfaceMethodInfo(expr ast.Expression) (interfaceMethodInfo, bool) {
	selection, ok := l.semanticSelection(expr)
	if !ok || !selection.Interface || selection.Kind != check.SelectionMethod && selection.Kind != check.SelectionMethodExpression {
		return interfaceMethodInfo{}, false
	}
	signature := selection.Signature
	if selection.Kind == check.SelectionMethodExpression {
		if len(signature.Params) == 0 {
			return interfaceMethodInfo{}, false
		}
		signature.Params = signature.Params[1:]
	}
	if !selection.Receiver.Valid() {
		return interfaceMethodInfo{}, false
	}
	return interfaceMethodInfo{
		InterfaceType: selection.Receiver, Method: selection.Name, Signature: signature,
	}, true
}

func (l *lowerer) semanticSelectionPath(selection check.Selection) ([]string, string, bool) {
	if l.semantic == nil {
		return nil, "", false
	}
	typ := selection.Receiver
	path := make([]string, 0, len(selection.Index))
	for _, index := range selection.Index {
		for l.semantic.Relations.View(typ).Shape() == types.Pointer {
			elem, ok := l.semantic.Relations.View(typ).Elem()
			if !ok {
				return nil, "", false
			}
			typ = elem
		}
		fields, ok := l.semantic.Relations.View(typ).StructFields()
		if !ok || index < 0 || index >= len(fields) || strings.TrimSpace(fields[index].Name) == "" {
			return nil, "", false
		}
		field := fields[index]
		path = append(path, strings.TrimSpace(field.Name))
		typ = field.Type
	}
	formatted := l.formatSemanticType(typ)
	return path, formatted, formatted != ""
}

func (l *lowerer) semanticExpressionSignature(expr ast.Expression) (types.FunctionSignature, bool) {
	if l.semantic == nil {
		return types.FunctionSignature{}, false
	}
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok || info.Signature == nil {
		return types.FunctionSignature{}, false
	}
	return *info.Signature, true
}

func (l *lowerer) semanticExpressionResults(expr ast.Expression) ([]string, bool) {
	if l.semantic == nil {
		return nil, false
	}
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok || len(info.Results) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(info.Results))
	for _, result := range info.Results {
		typ := l.formatSemanticType(result)
		if typ == "" {
			return nil, false
		}
		out = append(out, typ)
	}
	return out, true
}

func (l *lowerer) semanticExpressionType(expr ast.Expression) (string, bool) {
	if l.semantic == nil || expr.NodeID == 0 {
		return "", false
	}
	if expr.Kind == ast.ExprComposite {
		composite, ok := l.semantic.Composites[expr.NodeID]
		if ok && !composite.TypeExact {
			return "", false
		}
	}
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok || !info.Type.Valid() || info.Mode == check.ExprMultiValue || info.Mode == check.ExprNoValue {
		return "", false
	}
	if !l.semantic.TypeExact(info.Type) {
		return "", false
	}
	typ := l.formatSemanticType(info.Type)
	return typ, typ != ""
}

func (l *lowerer) semanticBinaryFact(expr ast.Expression) (result, leftTarget, rightTarget string, ok bool) {
	if l.semantic == nil || expr.NodeID == 0 || expr.Kind != ast.ExprBinary {
		return "", "", "", false
	}
	info, found := l.semantic.Exprs[expr.NodeID]
	if !found || !info.Type.Valid() || !info.LeftTarget.Valid() || !info.RightTarget.Valid() {
		return "", "", "", false
	}
	result = l.formatSemanticType(info.Type)
	leftTarget = l.formatSemanticType(info.LeftTarget)
	rightTarget = l.formatSemanticType(info.RightTarget)
	return result, leftTarget, rightTarget, result != "" && leftTarget != "" && rightTarget != ""
}

func (l *lowerer) semanticUntypedConstant(expr ast.Expression) (bool, bool) {
	if l.semantic == nil || expr.NodeID == 0 {
		return false, false
	}
	if value, ok := l.semantic.Constants[expr.NodeID]; ok {
		return value.Untyped, true
	}
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok {
		return false, false
	}
	return info.Untyped, true
}

func (l *lowerer) semanticValueFact(expr ast.Expression) (string, check.ValueCategory, bool) {
	if l.semantic == nil {
		return "", check.ValueInvalid, false
	}
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok || !info.Type.Valid() || info.Category == check.ValueInvalid {
		return "", check.ValueInvalid, false
	}
	typ := l.formatSemanticType(info.Type)
	if typ == "" {
		return "", check.ValueInvalid, false
	}
	return typ, info.Category, true
}

func (l *lowerer) semanticCompositeFact(expr ast.Expression) (check.CompositeInfo, bool) {
	if l.semantic == nil || expr.Kind != ast.ExprComposite {
		return check.CompositeInfo{}, false
	}
	info, ok := l.semantic.Composites[expr.NodeID]
	return info, ok && info.Shape != types.Invalid
}

func (l *lowerer) semanticSwitchFact(stmt ast.Statement) (check.SwitchInfo, bool) {
	if l.semantic == nil || stmt.Kind != ast.StmtSwitch {
		return check.SwitchInfo{}, false
	}
	info, ok := l.semantic.Switches[stmt.NodeID]
	return info, ok && info.Kind != check.SwitchInvalid
}

func (l *lowerer) formatSemanticType(ref types.TypeRef) string {
	if text, ok := l.semanticTypeText[ref]; ok {
		return text
	}
	original := ref
	if l.semantic == nil || !ref.Valid() {
		return ""
	}
	ref = l.semantic.Relations.ResolveAlias(ref)
	text := types.FormatWithTable(l.semantic.TypeTable, ref)
	prefix := strings.TrimSpace(l.modulePath) + "."
	rewritten, ok := types.RewriteCanonicalText(text, func(name string) (string, bool) {
		name = strings.TrimSpace(name)
		if prefix != "." && strings.HasPrefix(name, prefix) {
			localName := strings.TrimPrefix(name, prefix)
			if types.IsBuiltinTypeName(localName) {
				return name, true
			}
			return localName, true
		}
		return name, true
	})
	if !ok {
		return ""
	}
	text = l.resolveType(rewritten)
	// The checked table is immutable throughout lowering. Start caching only
	// after local aliases have been rewritten; each lowerer owns its entries.
	if l.semanticTypeText != nil && text != "" {
		l.semanticTypeText[original] = text
	}
	return text
}
