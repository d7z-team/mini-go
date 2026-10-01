package lower

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) channelElementType(typ string) string {
	typ = strings.TrimSpace(typ)
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	if !ok {
		return ""
	}
	_, elem, ok := view.Waitable()
	if !ok {
		return ""
	}
	return l.typeRefString(elem)
}

func (l *lowerer) sendChannelElementType(expr ast.Expression, scope *funcScope, code string) (string, bool) {
	typ := l.expressionType(expr, scope)
	channel, ok := l.channelTypeInfo(l.resolveNamedUnderlyingType(typ))
	if !ok || channel.direction == "recv" {
		l.add(code, "send target must be a send-capable channel", expr.Span)
		return "", false
	}
	return channel.elem, true
}

func (l *lowerer) isChannelType(typ string) bool {
	typ = strings.TrimSpace(typ)
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	return ok && view.Shape() == types.Waitable
}

func (l *lowerer) isArrayType(typ string) bool {
	_, _, ok := l.arrayTypeInfo(typ)
	return ok
}

func (l *lowerer) isSliceType(typ string) bool {
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	return ok && view.Shape() == types.Slice
}

func (l *lowerer) sliceResultType(typ string) string {
	if _, elem, ok := l.arrayTypeInfo(typ); ok {
		return "Slice<" + elem + ">"
	}
	if arrayType, ok := l.pointerArrayType(typ); ok {
		if _, elem, ok := l.arrayTypeInfo(arrayType); ok {
			return "Slice<" + elem + ">"
		}
	}
	return l.resolveType(typ)
}

func (l *lowerer) arrayTypeInfo(typ string) (int64, string, bool) {
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	if !ok {
		return 0, "", false
	}
	length, elem, ok := view.Array()
	if !ok {
		return 0, "", false
	}
	return length, l.typeRefString(elem), true
}

func (l *lowerer) pointerElementType(typ string) (string, bool) {
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	if !ok || view.Shape() != types.Pointer {
		return "", false
	}
	elem, ok := view.Elem()
	if !ok {
		return "", false
	}
	return l.typeRefString(elem), true
}

func (l *lowerer) isPointerType(typ string) bool {
	_, ok := l.pointerElementType(typ)
	return ok
}

func (l *lowerer) mapTypeInfo(typ string) (string, string, bool) {
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	if !ok {
		return "", "", false
	}
	key, elem, ok := view.Map()
	if !ok {
		return "", "", false
	}
	return l.typeRefString(key), l.typeRefString(elem), true
}

func arrayTypeString(length int64, elem string) string {
	return "Array<" + strconv.FormatInt(length, 10) + ", " + elem + ">"
}

func (l *lowerer) isStringType(typ string) bool {
	return l.resolveNamedUnderlyingType(typ) == "String"
}

func (l *lowerer) resolveNamedUnderlyingType(typ string) string {
	typ = l.resolveType(strings.TrimSpace(typ))
	if typ == "" {
		return ""
	}
	if underlying, ok := l.namedUnderlyingTypes[typ]; ok {
		return underlying
	}
	underlying := typ
	if decl, ok := l.typeDeclFor(typ); ok {
		if resolved := l.resolveSourceType(decl); resolved != "" {
			underlying = resolved
		}
	} else if export, ok := l.importedTypeInfo(typ); ok {
		if resolved := l.resolveType(export.Underlying); resolved != "" {
			underlying = resolved
		}
	}
	if l.namedUnderlyingTypes != nil {
		l.namedUnderlyingTypes[typ] = underlying
	}
	return underlying
}

func arrayLengthTypeString(expr *ast.Expression) string {
	if expr == nil {
		return ""
	}
	if expr.Kind == ast.ExprLiteral {
		return strings.TrimSpace(expr.Literal)
	}
	if expr.Kind == ast.ExprIdent {
		return strings.TrimSpace(expr.Name)
	}
	return ""
}

func (l *lowerer) memberType(receiverType, field string) string {
	field = strings.TrimSpace(field)
	if field == "" {
		return ""
	}
	receiverType = l.resolveType(receiverType)
	if elem, ok := l.pointerElementType(receiverType); ok {
		receiverType = l.resolveType(elem)
	}
	if decl, ok := l.typeDecls[receiverType]; ok && decl.Kind == ast.TypeStruct {
		for _, candidate := range decl.Fields {
			if l.structFieldName(candidate) == field {
				return l.resolveSourceType(candidate.Type)
			}
		}
	}
	if export, ok := l.importedTypeInfo(receiverType); ok {
		if typ := l.structMemberType(export.Underlying, field); typ != "" {
			return typ
		}
	}
	return l.structMemberType(receiverType, field)
}

func (l *lowerer) structMemberType(typ, field string) string {
	view, ok := l.typeView(l.resolveNamedUnderlyingType(typ))
	if !ok || view.Shape() != types.Struct {
		return ""
	}
	fields, ok := view.StructFields()
	if !ok {
		return ""
	}
	for _, candidate := range fields {
		if strings.TrimSpace(candidate.Name) == field {
			return l.resolveType(l.typeRefString(candidate.Type))
		}
	}
	return ""
}

func (l *lowerer) importedTypeInfo(typ string) (moduleExportInfo, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return moduleExportInfo{}, false
	}
	info, ok := l.importedTypes[typ]
	return info, ok
}

func formatCanonicalStructField(name, typ, tag string, embedded bool) string {
	prefix := ""
	if embedded {
		prefix = "embedded "
	}
	out := prefix + strings.TrimSpace(name) + ":" + strings.TrimSpace(typ)
	if tag != "" {
		out += " `" + tag + "`"
	}
	return out
}

func typeString(typ ast.TypeExpr) string {
	switch typ.Kind {
	case ast.TypeName:
		return typ.Name
	case ast.TypePointer:
		if typ.Elem == nil {
			return "Ptr<Any>"
		}
		return "Ptr<" + typeString(*typ.Elem) + ">"
	case ast.TypeSlice:
		if typ.Elem == nil {
			return "Slice<Any>"
		}
		return "Slice<" + typeString(*typ.Elem) + ">"
	case ast.TypeArray:
		length := arrayLengthTypeString(typ.Len)
		if typ.LenInfer {
			length = "..."
		}
		if length == "" {
			length = "?"
		}
		if typ.Elem == nil {
			return "Array<" + length + ", Any>"
		}
		return "Array<" + length + ", " + typeString(*typ.Elem) + ">"
	case ast.TypeMap:
		if typ.Key == nil || typ.Elem == nil {
			return "Map<Any, Any>"
		}
		return "Map<" + typeString(*typ.Key) + ", " + typeString(*typ.Elem) + ">"
	case ast.TypeChan:
		if typ.Elem == nil {
			return "Waitable<Any>"
		}
		switch typ.Direction {
		case "recv":
			return "ReceiveWaitable<" + typeString(*typ.Elem) + ">"
		case "send":
			return "SendWaitable<" + typeString(*typ.Elem) + ">"
		default:
			return "Waitable<" + typeString(*typ.Elem) + ">"
		}
	case ast.TypeFunc:
		return canonicalFunctionTypeString(typ.Params, typ.Results)
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
			fields = append(fields, formatCanonicalStructField(name, typeString(field.Type), field.Tag, strings.TrimSpace(field.Name) == ""))
		}
		return "struct{" + strings.Join(fields, ",") + "}"
	case ast.TypeInterface:
		if len(typ.Embeds) == 0 && len(typ.Terms) == 0 && len(typ.Methods) == 0 {
			return "interface{}"
		}
		methods := make([]string, 0, len(typ.Embeds)+len(typ.Terms)+len(typ.Methods))
		for _, embed := range typ.Embeds {
			methods = append(methods, typeString(embed))
		}
		if len(typ.Terms) != 0 {
			terms := make([]string, 0, len(typ.Terms))
			for _, term := range typ.Terms {
				prefix := ""
				if term.Approx {
					prefix = "~"
				}
				terms = append(terms, prefix+typeString(term.Type))
			}
			methods = append(methods, strings.Join(terms, "|"))
		}
		for _, method := range typ.Methods {
			methods = append(methods, strings.TrimSpace(method.Name)+":"+signatureOf(method))
		}
		return "interface{" + strings.Join(methods, ",") + "}"
	default:
		return ""
	}
}

func canonicalFunctionTypeString(params, results []ast.Field) string {
	parts := make([]string, 0, len(params))
	for i, param := range params {
		typ := fieldTypeString(param)
		if i == len(params)-1 && param.Variadic {
			typ = canonicalVariadicFunctionParam + typ
		}
		parts = append(parts, typ)
	}
	resultTypes := make([]string, 0, len(results))
	for _, result := range results {
		resultTypes = append(resultTypes, typeString(result.Type))
	}
	resultText := "Void"
	if len(resultTypes) == 1 {
		resultText = resultTypes[0]
	} else if len(resultTypes) > 1 {
		resultText = "tuple(" + strings.Join(resultTypes, ", ") + ")"
	}
	return "function(" + strings.Join(parts, ", ") + ") " + resultText
}

func (l *lowerer) inferredCompositeTypeExpr(canonical string, span source.Span) ast.TypeExpr {
	canonical = strings.TrimSpace(canonical)
	ref, ok := l.typeRef(canonical)
	if !ok || ref.Kind == types.Named {
		return ast.TypeExpr{Kind: ast.TypeName, Name: canonical, Span: span}
	}
	view := types.View(l.typeTable, ref)
	if elemRef, ok := view.Elem(); ok && view.Shape() == types.Slice {
		elem := l.inferredCompositeTypeExpr(l.typeRefString(elemRef), span)
		return ast.TypeExpr{Kind: ast.TypeSlice, Elem: &elem, Span: span}
	}
	if length, elemRef, ok := view.Array(); ok {
		lengthExpr := ast.Expression{Kind: ast.ExprLiteral, Literal: strconv.FormatInt(length, 10), Type: &ast.TypeExpr{Kind: ast.TypeName, Name: "Int", Span: span}, Span: span}
		elem := l.inferredCompositeTypeExpr(l.typeRefString(elemRef), span)
		return ast.TypeExpr{Kind: ast.TypeArray, Len: &lengthExpr, Elem: &elem, Span: span}
	}
	if keyRef, elemRef, ok := view.Map(); ok {
		key := l.inferredCompositeTypeExpr(l.typeRefString(keyRef), span)
		elem := l.inferredCompositeTypeExpr(l.typeRefString(elemRef), span)
		return ast.TypeExpr{Kind: ast.TypeMap, Key: &key, Elem: &elem, Span: span}
	}
	if fieldRefs, ok := view.StructFields(); ok {
		fields := make([]ast.Field, 0, len(fieldRefs))
		for _, field := range fieldRefs {
			fields = append(fields, ast.Field{
				Name: field.Name,
				Type: l.inferredCompositeTypeExpr(l.typeRefString(field.Type), span),
				Tag:  field.Tag,
				Span: span,
			})
		}
		return ast.TypeExpr{Kind: ast.TypeStruct, Fields: fields, Span: span}
	}
	return ast.TypeExpr{Kind: ast.TypeName, Name: canonical, Span: span}
}

func astEmbeddedFieldName(typ ast.TypeExpr) string {
	if typ.Kind == ast.TypePointer && typ.Elem != nil {
		return astEmbeddedFieldName(*typ.Elem)
	}
	name := strings.TrimSpace(typ.Name)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if strings.ContainsAny(name, "<>{}(), ") {
		return ""
	}
	return name
}

func (l *lowerer) structFieldName(field ast.Field) string {
	name := strings.TrimSpace(field.Name)
	if name != "" {
		return name
	}
	if field.EmbeddedName != "" {
		return field.EmbeddedName
	}
	return l.embeddedFieldName(field.Type)
}

func (l *lowerer) embeddedFieldName(typ ast.TypeExpr) string {
	if typ.Kind == ast.TypePointer && typ.Elem != nil {
		return l.embeddedFieldName(*typ.Elem)
	}
	name := strings.TrimSpace(typ.Name)
	if name == "" {
		return ""
	}
	if elem, ok := l.pointerElementType(name); ok {
		return l.embeddedFieldName(ast.TypeExpr{Kind: ast.TypeName, Name: elem})
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if strings.ContainsAny(name, "<>{}(), ") {
		return ""
	}
	return name
}

func firstNonEmpty(left, right string) string {
	if strings.TrimSpace(left) != "" {
		return left
	}
	return right
}

func importAlias(decl ast.ImportDecl) string {
	alias := strings.TrimSpace(decl.Alias)
	if alias != "" {
		return alias
	}
	path := strings.Trim(strings.TrimSpace(decl.Path), "/")
	if path == "" {
		return ""
	}
	i := strings.LastIndex(path, "/")
	if i >= 0 {
		return path[i+1:]
	}
	return path
}
