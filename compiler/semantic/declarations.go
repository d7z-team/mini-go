package semantic

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (a *analyzer) predeclareFunctionSignature(decl *ast.FuncDecl, scope ScopeID) {
	if len(decl.TypeParams) != 0 {
		scope = a.newScope(ScopeType, decl.NodeID, scope)
		a.declareTypeParams(decl.TypeParams, scope)
	}
	for i := range decl.Params {
		a.analyzeType(&decl.Params[i].Type, scope)
	}
	for i := range decl.Results {
		a.analyzeType(&decl.Results[i].Type, scope)
	}
	object, ok := a.info.Lookup(scope, decl.Name)
	if !ok || object.Kind != ObjectFunc {
		return
	}
	signature := a.functionTypeSignature(decl.Params, decl.Results)
	if !validFunctionSignature(signature) {
		return
	}
	object.Type = a.storeFunctionType(decl.NodeID, "function", signature)
	a.info.Objects[object.ID] = object
	if len(decl.TypeParams) != 0 {
		a.info.GenericDecls[object.ID] = a.typeParamObjects(decl.TypeParams)
	}
}

func (a *analyzer) predeclareMethodSignature(decl *ast.FuncDecl, scope ScopeID) {
	if decl.Receiver == nil {
		return
	}
	scope = a.newScope(ScopeType, decl.NodeID, scope)
	a.declareReceiverTypeParams(decl.Receiver, scope)
	a.analyzeType(&decl.Receiver.Type, scope)
	for i := range decl.Params {
		a.analyzeType(&decl.Params[i].Type, scope)
	}
	for i := range decl.Results {
		a.analyzeType(&decl.Results[i].Type, scope)
	}
	receiver := a.resolvedType(decl.Receiver.Type)
	signature := a.functionTypeSignature(decl.Params, decl.Results)
	if !receiver.Valid() || !validFunctionSignature(signature) {
		return
	}
	owner := receiver
	if elem, ok := a.info.Relations.View(receiver).Elem(); ok && a.info.Relations.View(receiver).Shape() == types.Pointer {
		owner = elem
	}
	node, ok := a.info.TypeTable.Node(owner)
	if !ok || node.Kind != types.Named {
		return
	}
	node.Methods = append(node.Methods, types.Method{
		Name: strings.TrimSpace(decl.Name), Receiver: receiver, Signature: signature,
		ModulePath: a.info.ModulePath,
	})
	_ = a.info.TypeTable.Replace(node)
}

func (a *analyzer) analyzeDecl(decl *ast.Decl, scope ScopeID, local bool) {
	switch decl.Kind {
	case ast.DeclConst, ast.DeclVar:
		value := &decl.Const
		kind := ObjectConst
		mutable := false
		if decl.Kind == ast.DeclVar {
			value = &decl.Var
			kind = ObjectVar
			mutable = true
		}
		a.analyzeType(&value.Type, scope)
		for i := range value.Values {
			if value.Values[i].Kind == ast.ExprEmbed {
				a.analyzeEmbedInitializer(value, &value.Values[i], local, decl.Span)
			} else {
				a.analyzeExpr(&value.Values[i], scope)
				if a.info.Exprs[value.Values[i].NodeID].Mode == ExprNoValue {
					a.addDiagnostic("semantic.decl.no_value", "declaration expression produces no value", value.Values[i].Span)
				}
			}
		}
		declaredType := a.resolvedType(value.Type)
		a.validateValueType(declaredType, value.Type.Span)
		inferredTypes := a.assignmentExpressionTypes(value.Values, len(value.Names))
		if declaredType.Valid() && len(value.Values) != 0 {
			targets := make([]types.TypeRef, len(value.Names))
			for i := range targets {
				targets[i] = declaredType
			}
			a.validateAssignments(value.Values, targets)
		} else if kind == ObjectVar {
			a.validateAssignments(value.Values, inferredTypes)
		}
		if len(value.Values) != 0 && len(inferredTypes) != len(value.Names) {
			a.addDiagnostic("semantic.decl.value_count", "declaration value count does not match name count", decl.Span)
		}
		if local {
			for i, name := range value.Names {
				typ := declaredType
				if !typ.Valid() && i < len(inferredTypes) {
					typ = inferredTypes[i]
				}
				a.declare(scope, kind, name, decl.NodeID, typ, mutable, false)
				if object, ok := a.info.Lookup(scope, name); ok && object.Scope == scope && object.Kind == kind {
					object.Untyped = kind == ObjectConst && !declaredType.Valid() && i < len(value.Values) && a.info.Exprs[value.Values[i].NodeID].Untyped
					a.info.Objects[object.ID] = object
				}
			}
		} else {
			for i, name := range value.Names {
				if object, ok := a.info.Lookup(scope, name); ok && object.Kind == kind {
					typ := declaredType
					if !typ.Valid() && i < len(inferredTypes) {
						typ = inferredTypes[i]
					}
					if typ.Valid() {
						object.Type = typ
					}
					object.Untyped = kind == ObjectConst && !declaredType.Valid() && i < len(value.Values) && a.info.Exprs[value.Values[i].NodeID].Untyped
					a.info.Objects[object.ID] = object
				}
			}
		}
		if kind == ObjectConst {
			a.bindAnalyzedConstants(*value, scope)
		}
	case ast.DeclType:
		if local {
			name := decl.Type.Name
			canonical := "local_type_" + strconv.FormatUint(uint64(decl.NodeID), 10) + "_" + name
			id := types.TypeID(canonical)
			key := types.TypeKey{ModulePath: a.info.ModulePath, DeclID: types.DeclID(canonical)}
			_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.Named, Identity: key, Underlying: types.AnyType()})
			a.declare(scope, ObjectType, name, decl.NodeID, types.TypeRef{Kind: types.Named, Named: key, Node: id}, false, decl.Type.Alias)
		}
		typeScope := scope
		if len(decl.Type.TypeParams) != 0 {
			typeScope = a.newScope(ScopeType, decl.NodeID, scope)
			a.declareTypeParams(decl.Type.TypeParams, typeScope)
			if object, ok := a.info.Lookup(scope, decl.Type.Name); ok {
				a.info.GenericDecls[object.ID] = a.typeParamObjects(decl.Type.TypeParams)
			}
		}
		a.analyzeType(&decl.Type.Type, typeScope)
		if object, ok := a.info.Lookup(scope, decl.Type.Name); ok && object.Type.Node != "" {
			underlying := a.resolvedType(decl.Type.Type)
			if node, exists := a.info.TypeTable.Node(object.Type); exists && underlying.Valid() {
				node.Alias = decl.Type.Alias
				if decl.Type.Alias {
					node.AliasTarget, node.Underlying = underlying, types.TypeRef{}
				} else {
					node.Underlying, node.AliasTarget = underlying, types.TypeRef{}
				}
				_ = a.info.TypeTable.Replace(node)
				if local {
					a.validateTypeCycle(object, decl.Span)
				}
			}
		}
	case ast.DeclFunc:
		objectID := ObjectID("")
		if decl.Func.Receiver == nil {
			if object, ok := a.info.Lookup(scope, decl.Func.Name); ok && object.Kind == ObjectFunc {
				objectID = object.ID
			}
		}
		a.analyzeFunc(&decl.Func, scope, objectID)
	}
}

func (a *analyzer) analyzeFunc(decl *ast.FuncDecl, parent ScopeID, objectID ObjectID) {
	scope := a.newScope(ScopeFunction, decl.NodeID, parent)
	a.declareTypeParams(decl.TypeParams, scope)
	a.declareReceiverTypeParams(decl.Receiver, scope)
	if decl.Receiver != nil {
		a.analyzeField(decl.Receiver, scope, true)
	}
	for i := range decl.Params {
		a.analyzeField(&decl.Params[i], scope, true)
	}
	for i := range decl.Results {
		a.analyzeField(&decl.Results[i], scope, true)
	}
	signature := a.functionTypeSignature(decl.Params, decl.Results)
	if objectID != "" {
		object := a.info.Objects[objectID]
		if validFunctionSignature(signature) {
			object.Type = a.storeFunctionType(decl.NodeID, "function", signature)
		}
		a.info.Objects[objectID] = object
		a.info.Functions[decl.NodeID] = objectID
		if len(decl.TypeParams) != 0 {
			a.info.GenericDecls[objectID] = a.typeParamObjects(decl.TypeParams)
		}
	}
	previousResults := a.resultTypes
	a.resultTypes = signature.Results
	a.analyzeBlock(&decl.Body, scope, false)
	a.resultTypes = previousResults
}

func (a *analyzer) storeFunctionType(nodeID ast.NodeID, role string, signature types.FunctionSignature) types.TypeRef {
	id := types.TypeID(role + "." + strconv.FormatUint(uint64(nodeID), 10))
	node := types.TypeNode{ID: id, Kind: types.Function, Signature: &signature}
	ref := types.TypeRef{Kind: types.Function, Node: id}
	if _, ok := a.info.TypeTable.Node(ref); ok {
		_ = a.info.TypeTable.Replace(node)
	} else {
		_ = a.info.TypeTable.Add(node)
	}
	return ref
}

func validFunctionSignature(signature types.FunctionSignature) bool {
	for _, param := range signature.Params {
		if !param.Type.Valid() {
			return false
		}
	}
	for _, result := range signature.Results {
		if !result.Valid() {
			return false
		}
	}
	return true
}

func (a *analyzer) typeParamObjects(params []ast.TypeParam) []ObjectID {
	objects := make([]ObjectID, 0, len(params))
	for i := range params {
		defs := a.info.Defs[params[i].NodeID]
		if len(defs) != 0 {
			objects = append(objects, defs[len(defs)-1])
		}
	}
	return objects
}

func (a *analyzer) declareTypeParams(params []ast.TypeParam, scope ScopeID) {
	for i := range params {
		param := &params[i]
		if param.Name == "_" {
			continue
		}
		id := types.TypeID("typeparam." + strconv.FormatUint(uint64(param.NodeID), 10))
		ref := types.TypeRef{Kind: types.TypeParameter, Node: id}
		_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.TypeParameter, Name: param.Name, Constraint: types.AnyType()})
		a.declare(scope, ObjectTypeParam, param.Name, param.NodeID, ref, false, false)
	}
	for i := range params {
		a.analyzeType(&params[i].Constraint, scope)
		a.validateTypeSetConstraint(params[i].Constraint)
		if params[i].Name == "_" {
			continue
		}
		defs := a.info.Defs[params[i].NodeID]
		if len(defs) == 0 {
			continue
		}
		object := a.info.Objects[defs[len(defs)-1]]
		node, ok := a.info.TypeTable.Node(object.Type)
		if !ok {
			continue
		}
		if constraint := a.resolvedType(params[i].Constraint); constraint.Valid() {
			node.Constraint = constraint
			_ = a.info.TypeTable.Replace(node)
		}
	}
}

func (a *analyzer) declareReceiverTypeParams(receiver *ast.Field, scope ScopeID) {
	if receiver == nil {
		return
	}
	typ := &receiver.Type
	if typ.Kind == ast.TypePointer && typ.Elem != nil {
		typ = typ.Elem
	}
	if typ.Kind != ast.TypeInstance {
		return
	}
	for i := range typ.TypeArgs {
		arg := &typ.TypeArgs[i]
		if arg.Kind != ast.TypeName || strings.Contains(arg.Name, ".") {
			continue
		}
		id := types.TypeID("receiver_typeparam." + strconv.FormatUint(uint64(arg.NodeID), 10))
		ref := types.TypeRef{Kind: types.TypeParameter, Node: id}
		_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.TypeParameter, Name: arg.Name, Constraint: types.AnyType()})
		a.declare(scope, ObjectTypeParam, arg.Name, arg.NodeID, ref, false, false)
	}
}

func (a *analyzer) analyzeField(field *ast.Field, scope ScopeID, declareName bool) {
	a.analyzeType(&field.Type, scope)
	if declareName {
		a.validateValueType(a.resolvedType(field.Type), field.Span)
		a.declare(scope, ObjectVar, field.Name, field.NodeID, a.resolvedType(field.Type), true, false)
	}
}
