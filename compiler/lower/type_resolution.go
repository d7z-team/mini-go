package lower

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) compositeKind(expr ast.Expression) string {
	if info, ok := l.semanticCompositeFact(expr); ok {
		switch info.Shape {
		case types.Array, types.Slice:
			return "array"
		case types.Map:
			return "map"
		case types.Struct:
			return "struct"
		}
	}
	typ := l.resolveSourceTypePtr(expr.Type, nil)
	if expr.Type != nil {
		switch expr.Type.Kind {
		case ast.TypeArray, ast.TypeSlice:
			return "array"
		case ast.TypeMap:
			return "map"
		case ast.TypeStruct:
			return "struct"
		}
	}
	if view, ok := l.typeView(l.resolveNamedUnderlyingType(typ)); ok {
		switch view.Shape() {
		case types.Array, types.Slice:
			return "array"
		case types.Map:
			return "map"
		case types.Struct:
			return "struct"
		}
	}
	if decl, ok := l.typeDecls[typ]; ok {
		switch decl.Kind {
		case ast.TypeArray, ast.TypeSlice:
			return "array"
		case ast.TypeMap:
			return "map"
		case ast.TypeStruct:
			return "struct"
		}
	}
	if len(expr.Items) != 0 {
		allIdentKeys := true
		for _, entry := range expr.Items {
			if entry.Key == nil || entry.Key.Kind != ast.ExprIdent {
				allIdentKeys = false
				break
			}
		}
		if allIdentKeys {
			return "struct"
		}
	}
	return ""
}

func (l *lowerer) rangeKind(expr ast.Expression, scope *funcScope) string {
	typ := l.expressionType(expr, scope)
	if _, _, ok := l.mapKeyValueTypes(typ); ok {
		return "map"
	}
	if l.isChannelType(typ) {
		return "chan"
	}
	if l.isIntegerType(l.resolveNamedUnderlyingType(typ)) {
		return "integer"
	}
	if l.isFunctionSignatureType(typ) {
		return "function"
	}
	if l.isArrayType(typ) || l.isSliceType(typ) || l.isStringType(typ) || l.pointerArrayTypeOK(typ) {
		return "indexable"
	}
	return "invalid"
}

func (l *lowerer) pointerArrayTypeOK(typ string) bool {
	_, ok := l.pointerArrayType(typ)
	return ok
}

func (l *lowerer) pointerArrayType(typ string) (string, bool) {
	resolved := l.resolveNamedUnderlyingType(typ)
	view, ok := l.typeView(resolved)
	if !ok {
		return "", false
	}
	elem, ok := view.Elem()
	if !ok || view.Shape() != types.Pointer {
		return "", false
	}
	length, arrayElem, ok := l.arrayTypeInfo(l.typeRefString(elem))
	if !ok {
		return "", false
	}
	return arrayTypeString(length, arrayElem), true
}

func (l *lowerer) resolveType(typ string) string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return ""
	}
	if resolved, ok := l.resolvedTypes[typ]; ok {
		return resolved
	}
	resolved, ok := types.RewriteCanonicalText(typ, l.resolveTypeName)
	if !ok || strings.TrimSpace(resolved) == "" {
		if l.resolvedTypes != nil {
			l.resolvedTypes[typ] = typ
		}
		return typ
	}
	if l.resolvedTypes != nil {
		l.resolvedTypes[typ] = resolved
	}
	return resolved
}

func (l *lowerer) resolveTypeName(typ string) (string, bool) {
	return l.resolveTypeNameInFile(typ, source.Span{})
}

func (l *lowerer) resolveTypeNameInFile(typ string, span source.Span) (string, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return "", false
	}
	if _, ok := l.typeDecls[typ]; ok {
		return typ, true
	}
	original := typ
	for i := 0; i < 32; i++ {
		target, ok := l.typeAliases[typ]
		if !ok {
			break
		}
		target = strings.TrimSpace(target)
		if target == "" || target == typ {
			break
		}
		typ = target
	}
	if typ != original {
		return typ, true
	}
	if imported, ok := l.dotImportExport(typ, span); ok && imported.Kind == check.ObjectType {
		return l.importedTypeCanonicalName(typ, imported), true
	}
	if canonical, ok := sourcePredeclaredType(typ); ok {
		return canonical, true
	}
	if imported, ok := l.resolveImportedTypeSelectorInFile(typ, span); ok {
		return imported, true
	}
	return "", false
}

func sourcePredeclaredType(name string) (string, bool) {
	switch strings.TrimSpace(name) {
	case "bool":
		return "Bool", true
	case "string":
		return "String", true
	case "int":
		return "Int", true
	case "int8":
		return "Int8", true
	case "int16":
		return "Int16", true
	case "int32", "rune":
		return "Int32", true
	case "int64":
		return "Int64", true
	case "uint":
		return "Uint", true
	case "uint8", "byte":
		return "Uint8", true
	case "uint16":
		return "Uint16", true
	case "uint32":
		return "Uint32", true
	case "uint64":
		return "Uint64", true
	case "uintptr":
		return "Uintptr", true
	case "float32":
		return "Float32", true
	case "float64":
		return "Float64", true
	case "complex64":
		return "Complex64", true
	case "complex128":
		return "Complex128", true
	case "any":
		return "Any", true
	case "error":
		return "interface{Error:function() String}", true
	default:
		return "", false
	}
}

func (l *lowerer) resolveImportedTypeSelectorInFile(typ string, span source.Span) (string, bool) {
	dot := strings.Index(typ, ".")
	if dot <= 0 || dot == len(typ)-1 {
		return "", false
	}
	alias := strings.TrimSpace(typ[:dot])
	name := strings.TrimSpace(typ[dot+1:])
	modulePath, ok := l.importPathForAlias(alias, span)
	if !ok {
		return "", false
	}
	export, ok := l.moduleExports[modulePath][name]
	if !ok || export.Kind != check.ObjectType {
		return "", false
	}
	l.markImportExport(modulePath, name)
	if export.Type != "" {
		return export.Type, true
	}
	if export.Underlying != "" {
		return export.Underlying, true
	}
	return modulePath + "." + name, true
}

func (l *lowerer) resolveSourceType(typ ast.TypeExpr) string {
	return l.resolveSourceTypeInScope(typ, nil)
}

func (l *lowerer) resolveSourceTypePtr(typ *ast.TypeExpr, scope *funcScope) string {
	if typ == nil {
		return ""
	}
	return l.resolveSourceTypeInScope(*typ, scope)
}

func (l *lowerer) resolveSourceTypeInScope(typ ast.TypeExpr, scope *funcScope) string {
	if l.semantic != nil {
		if info, ok := l.semantic.Types[typ.NodeID]; ok && info.Type.Valid() {
			if typ.Kind == ast.TypeName && info.Type.Kind == types.Named && info.Type.Named.ModulePath == l.modulePath && types.IsBuiltinTypeName(string(info.Type.Named.DeclID)) {
				node, found := l.semantic.TypeTable.Node(info.Type)
				if !found || !node.Alias {
					return info.Type.Named.ModulePath + "." + string(info.Type.Named.DeclID)
				}
			}
		}
	}
	switch typ.Kind {
	case ast.TypeName:
		name := strings.TrimSpace(typ.Name)
		if name == "" {
			return ""
		}
		if resolved, ok := l.resolveTypeNameInFile(name, typ.Span); ok {
			return resolved
		}
		return name
	case ast.TypePointer:
		if typ.Elem == nil {
			return "Ptr<Any>"
		}
		return "Ptr<" + l.resolveSourceTypeInScope(*typ.Elem, scope) + ">"
	case ast.TypeSlice:
		if typ.Elem == nil {
			return "Slice<Any>"
		}
		return "Slice<" + l.resolveSourceTypeInScope(*typ.Elem, scope) + ">"
	case ast.TypeArray:
		length := l.arrayLengthTypeString(typ.Len, scope)
		if typ.LenInfer {
			length = "..."
		}
		if length == "" {
			return ""
		}
		if typ.Elem == nil {
			return "Array<" + length + ", Any>"
		}
		return "Array<" + length + ", " + l.resolveSourceTypeInScope(*typ.Elem, scope) + ">"
	case ast.TypeMap:
		if typ.Key == nil || typ.Elem == nil {
			return "Map<Any, Any>"
		}
		return "Map<" + l.resolveSourceTypeInScope(*typ.Key, scope) + ", " + l.resolveSourceTypeInScope(*typ.Elem, scope) + ">"
	case ast.TypeChan:
		elem := "Any"
		if typ.Elem != nil {
			elem = l.resolveSourceTypeInScope(*typ.Elem, scope)
		}
		switch typ.Direction {
		case "recv":
			return "ReceiveWaitable<" + elem + ">"
		case "send":
			return "SendWaitable<" + elem + ">"
		default:
			return "Waitable<" + elem + ">"
		}
	case ast.TypeFunc:
		return l.sourceFunctionTypeString(typ.Params, typ.Results)
	case ast.TypeStruct:
		fields := make([]string, 0, len(typ.Fields))
		for _, field := range typ.Fields {
			name := strings.TrimSpace(field.Name)
			if name == "" {
				name = astEmbeddedFieldName(field.Type)
			}
			if name == "" {
				name = "_"
			}
			fields = append(fields, formatCanonicalStructField(name, l.resolveSourceTypeInScope(field.Type, scope), field.Tag, strings.TrimSpace(field.Name) == ""))
		}
		return "struct{" + strings.Join(fields, ",") + "}"
	case ast.TypeInterface:
		if len(typ.Embeds) == 0 && len(typ.Terms) == 0 && len(typ.Methods) == 0 {
			return "interface{}"
		}
		methods := make([]string, 0, len(typ.Embeds)+len(typ.Terms)+len(typ.Methods))
		for _, embed := range typ.Embeds {
			methods = append(methods, l.resolveSourceTypeInScope(embed, scope))
		}
		if len(typ.Terms) != 0 {
			terms := make([]string, 0, len(typ.Terms))
			for _, term := range typ.Terms {
				prefix := ""
				if term.Approx {
					prefix = "~"
				}
				terms = append(terms, prefix+l.resolveSourceTypeInScope(term.Type, scope))
			}
			methods = append(methods, strings.Join(terms, "|"))
		}
		for _, method := range typ.Methods {
			methods = append(methods, strings.TrimSpace(method.Name)+":"+l.signatureOf(method))
		}
		return "interface{" + strings.Join(methods, ",") + "}"
	default:
		return l.resolveType(typeString(typ))
	}
}

func (l *lowerer) arrayLengthTypeString(expr *ast.Expression, scope *funcScope) string {
	if expr == nil {
		return ""
	}
	if l.semantic != nil {
		if length, ok := l.semantic.ArrayLengths[expr.NodeID]; ok {
			return strconv.FormatInt(length, 10)
		}
	}
	if value, ok := l.arrayLengthConst(expr, scope); ok {
		return strconv.FormatInt(value, 10)
	}
	return ""
}

func (l *lowerer) arrayLengthConst(expr *ast.Expression, scope *funcScope) (int64, bool) {
	if expr == nil {
		return 0, false
	}
	if l.semantic != nil {
		if length, ok := l.semantic.ArrayLengths[expr.NodeID]; ok {
			return length, true
		}
	}
	raw, typ, ok := l.constValue(*expr, scope)
	if !ok {
		return 0, false
	}
	converted, convertedType, ok := l.convertConstValue(raw, typ, "Int")
	if !ok {
		return 0, false
	}
	length, ok := l.constInt64(converted, convertedType)
	if !ok || length < 0 {
		return 0, false
	}
	return length, true
}
