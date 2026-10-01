package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) lowerMethodExpression(expr ast.Expression) (ir.Expression, bool) {
	selection, selected := l.semanticSelection(expr)
	if !selected || selection.Kind != check.SelectionMethodExpression {
		return ir.Expression{}, false
	}
	method, interfaceMethod, ok := l.semanticMethodInfo(expr)
	if !ok || interfaceMethod {
		return ir.Expression{}, false
	}
	if len(selection.Index) == 0 && l.semantic.Relations.Identical(selection.Receiver, method.Receiver).OK {
		return ir.Expression{Kind: ir.ExprFunction, ModulePath: method.ModulePath, Function: method.FunctionID}, true
	}
	path, selectedType, ok := l.semanticSelectionPath(selection)
	if !ok {
		return ir.Expression{}, false
	}
	locals := make([]ir.Local, len(selection.Signature.Params))
	args := make([]ir.Expression, len(locals))
	for i, param := range selection.Signature.Params {
		locals[i] = ir.Local{ID: fmt.Sprintf("local.method.expr.arg.%d", i), Name: fmt.Sprintf("arg.%d", i), Type: param.Type, Generated: true}
		args[i] = ir.Expression{Kind: ir.ExprLocal, Local: locals[i].ID}
	}
	receiver := projectSelectorPath(args[0], path)
	if selection.Indirect {
		segments := []ir.AddressSegment{}
		typ := selection.Receiver
		for _, index := range selection.Index {
			view := l.semantic.Relations.View(typ)
			if view.Shape() == types.Pointer {
				segments = append(segments, ir.AddressSegment{Kind: "indirect"})
				typ, _ = view.Elem()
			}
			fields, _ := l.semantic.Relations.View(typ).StructFields()
			segments = append(segments, ir.AddressSegment{Kind: "field", Field: fields[index].Name})
			typ = fields[index].Type
		}
		receiver = ir.Expression{Kind: ir.ExprAddressOf, Local: locals[0].ID, Path: segments}
	}
	args[0] = l.autoDerefMethodReceiver(receiver, selectedType, method)
	call := ir.Expression{Kind: ir.ExprCallDirect, ModulePath: method.ModulePath, Function: method.FunctionID, Args: args, ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results}
	body := []ir.Statement{{Kind: ir.StmtReturn, Results: []ir.Expression{call}}}
	if len(method.Signature.Results) == 0 {
		body = []ir.Statement{{Kind: ir.StmtExpr, Expr: call}, {Kind: ir.StmtReturn}}
	}
	l.nextAnonFunc++
	fn := ir.Function{
		ID: fmt.Sprintf("fn.method.expr.%d", l.nextAnonFunc), Name: fmt.Sprintf("method.expr.%d", l.nextAnonFunc),
		RevisionLocal: true, Generated: true, Signature: selection.Signature, Locals: locals, Body: body,
		Declaration: hirLocationPtr(expr.Span),
	}
	l.extraFunctions = append(l.extraFunctions, fn)
	return ir.Expression{Kind: ir.ExprFunction, Function: fn.ID}, true
}

func (l *lowerer) lowerInterfaceMethodExpression(expr ast.Expression) (ir.Expression, bool) {
	selection, selected := l.semanticSelection(expr)
	if !selected || selection.Kind != check.SelectionMethodExpression {
		return ir.Expression{}, false
	}
	method, ok := l.semanticInterfaceMethodInfo(expr)
	if !ok {
		return ir.Expression{}, false
	}
	receiverLocal := ir.Local{ID: "local.interface.method.expr.recv", Name: "recv", Type: method.InterfaceType, Generated: true}
	args := make([]ir.Expression, 0, len(method.Signature.Params))
	locals := []ir.Local{receiverLocal}
	for i, param := range method.Signature.Params {
		local := ir.Local{
			ID:        fmt.Sprintf("local.interface.method.expr.arg.%d", i),
			Name:      fmt.Sprintf("arg.%d", i),
			Type:      param.Type,
			Generated: true,
		}
		locals = append(locals, local)
		args = append(args, ir.Expression{Kind: ir.ExprLocal, Local: local.ID})
	}
	receiverRef := ir.Expression{Kind: ir.ExprLocal, Local: receiverLocal.ID}
	call := ir.Expression{
		Kind:        ir.ExprCallInterface,
		Type:        method.InterfaceType,
		Field:       method.Method,
		Operand:     &receiverRef,
		Args:        args,
		ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
	}
	body := []ir.Statement{}
	if len(method.Signature.Results) == 0 {
		body = append(body,
			ir.Statement{Kind: ir.StmtExpr, Expr: call},
			ir.Statement{Kind: ir.StmtReturn},
		)
	} else {
		body = append(body, ir.Statement{Kind: ir.StmtReturn, Results: []ir.Expression{call}})
	}
	l.nextAnonFunc++
	fn := ir.Function{
		ID:            fmt.Sprintf("fn.interface.method.expr.%d", l.nextAnonFunc),
		Name:          fmt.Sprintf("interface.method.expr.%d", l.nextAnonFunc),
		RevisionLocal: true,
		Generated:     true,
		Signature: types.FunctionSignature{
			Params:   append([]types.TypeParam{{Type: method.InterfaceType}}, method.Signature.Params...),
			Results:  append([]types.TypeRef(nil), method.Signature.Results...),
			Variadic: method.Signature.Variadic,
		},
		Locals: locals,
		Body:   body,
	}
	l.extraFunctions = append(l.extraFunctions, fn)
	return ir.Expression{Kind: ir.ExprFunction, Function: fn.ID}, true
}

func (l *lowerer) methodExpressionReceiverType(expr ast.Expression, scope *funcScope) (string, bool) {
	switch expr.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if name == "" {
			return "", false
		}
		if scope != nil {
			if _, _, ok := l.lookupLocalOrConst(name, scope); ok {
				return "", false
			}
			if _, _, ok := l.lookupUpvalue(name, scope); ok {
				return "", false
			}
		}
		receiverType := l.resolveType(name)
		if _, ok := l.typeDecls[receiverType]; !ok {
			if _, ok := l.importedTypeInfo(receiverType); !ok {
				if _, ok := l.interfaceType(receiverType); !ok {
					return "", false
				}
			}
		}
		return receiverType, true
	case ast.ExprDeref:
		if expr.Operand == nil {
			return "", false
		}
		receiverType, ok := l.methodExpressionReceiverType(*expr.Operand, scope)
		if !ok {
			return "", false
		}
		return "Ptr<" + receiverType + ">", true
	case ast.ExprSelector:
		if expr.Operand == nil || strings.TrimSpace(expr.Field) == "" {
			return "", false
		}
		modulePath, ok := l.selectorModulePath(expr, scope)
		if !ok {
			return "", false
		}
		receiverType := strings.TrimSpace(modulePath) + "." + strings.TrimSpace(expr.Field)
		if _, ok := l.importedTypeInfo(receiverType); !ok {
			return "", false
		}
		l.markImportExport(modulePath, expr.Field)
		return receiverType, true
	default:
		return "", false
	}
}

func (l *lowerer) lowerMethodValue(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	selection, selected := l.semanticSelection(expr)
	if !selected || selection.Kind != check.SelectionMethod || expr.Operand == nil {
		return ir.Expression{}, false
	}
	var receiver ir.Expression
	var receiverType types.TypeRef
	var signature types.FunctionSignature
	var call ir.Expression
	prefix := "method"
	if selection.Interface {
		method, ok := l.semanticInterfaceMethodInfo(expr)
		if !ok {
			return ir.Expression{}, false
		}
		receiver, ok = l.lowerExpression(*expr.Operand, scope)
		if !ok {
			return ir.Expression{}, false
		}
		prefix = "interface.method"
		receiverType, signature = method.InterfaceType, method.Signature
		call = ir.Expression{Kind: ir.ExprCallInterface, Type: method.InterfaceType, Field: method.Method}
	} else {
		method, ok := l.methodInfoFromSelection(selection)
		if !ok {
			return ir.Expression{}, false
		}
		path, selectedType, ok := l.semanticSelectionPath(selection)
		if !ok {
			return ir.Expression{}, false
		}
		if selection.Indirect {
			receiver, ok = l.lowerAddressableReceiver(selectorPathExpression(*expr.Operand, path), scope)
		} else {
			receiver, ok = l.lowerExpression(*expr.Operand, scope)
			receiver = projectSelectorPath(receiver, path)
		}
		if !ok {
			return ir.Expression{}, false
		}
		receiver = l.autoDerefMethodReceiver(receiver, selectedType, method)
		receiverType, signature = method.Receiver, method.Signature
		call = ir.Expression{Kind: ir.ExprCallDirect, ModulePath: method.ModulePath, Function: method.FunctionID}
	}
	receiverLocal := l.newSyntheticLocal(scope, prefix+".recv", l.typeRefString(receiverType))
	receiverRef := ir.Expression{Kind: ir.ExprUpvalue, Upvalue: "up." + prefix + ".recv"}
	args := make([]ir.Expression, 0, len(signature.Params)+1)
	if selection.Interface {
		call.Operand = &receiverRef
	} else {
		args = append(args, receiverRef)
	}
	locals := make([]ir.Local, 0, len(signature.Params))
	for i, param := range signature.Params {
		local := ir.Local{
			ID: fmt.Sprintf("local.%s.arg.%d", prefix, i), Name: fmt.Sprintf("arg.%d", i),
			Type: param.Type, Generated: true,
		}
		locals = append(locals, local)
		args = append(args, ir.Expression{Kind: ir.ExprLocal, Local: local.ID})
	}
	call.Args, call.ResultCount = args, len(signature.Results)
	call.ResultTypes = signature.Results
	body := []ir.Statement{{Kind: ir.StmtReturn, Results: []ir.Expression{call}}}
	if len(signature.Results) == 0 {
		body = []ir.Statement{{Kind: ir.StmtExpr, Expr: call}, {Kind: ir.StmtReturn}}
	}
	l.nextAnonFunc++
	fn := ir.Function{
		ID:            fmt.Sprintf("fn.%s.value.%d", prefix, l.nextAnonFunc),
		Name:          fmt.Sprintf("%s.value.%d", prefix, l.nextAnonFunc),
		RevisionLocal: true, Generated: true, Signature: signature, Locals: locals,
		Upvalues: []ir.Upvalue{{ID: receiverRef.Upvalue, Name: prefix + ".recv", Type: receiverType}},
		Body:     body,
	}
	l.extraFunctions = append(l.extraFunctions, fn)
	fnValue := ir.Expression{
		Kind: ir.ExprFunction, Function: fn.ID,
		Captures: []ir.CaptureTarget{{Kind: "local", Local: receiverLocal}},
	}
	return ir.Expression{Kind: ir.ExprLet, Local: receiverLocal, Bind: &receiver, Body: &fnValue}, true
}
