package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) validateMapCompositeEntries(expr ast.Expression, keyType string, scope *funcScope) bool {
	seen := map[string]struct{}{}
	for _, entry := range expr.Items {
		if entry.Key == nil {
			l.add("hirgen.composite.map.key.missing", "map composite entry requires a key", expr.Span)
			return false
		}
		key, ok := l.switchCaseConstantKey(*entry.Key, keyType, scope)
		if !ok {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			l.add("hirgen.composite.map.duplicate", "map composite literal contains duplicate constant key", entry.Key.Span)
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func (l *lowerer) lowerComposite(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	typ := l.resolveSourceTypePtr(expr.Type, scope)
	composite, structured := l.semanticCompositeFact(expr)
	if structured && composite.Type.Valid() && composite.TypeExact {
		typ = l.formatSemanticType(composite.Type)
	}
	switch l.compositeKind(expr) {
	case "array":
		return l.lowerArrayCompositeItems(expr, scope, typ)
	case "map":
		keyType, valueType, _ := l.mapKeyValueTypes(typ)
		if structured {
			keyType = l.formatSemanticType(composite.Key)
			valueType = l.formatSemanticType(composite.Element)
		}
		if !l.validateMapKeyComparable(typ, expr.Span) {
			return ir.Expression{}, false
		}
		if !l.validateMapCompositeEntries(expr, keyType, scope) {
			return ir.Expression{}, false
		}
		entries := make([]ir.MapEntry, 0, len(expr.Items))
		for _, entry := range expr.Items {
			key, ok := l.lowerCompositeValueInType(*entry.Key, keyType, scope)
			if !ok {
				return ir.Expression{}, false
			}
			value, ok := l.lowerCompositeValueInType(entry.Value, valueType, scope)
			if !ok {
				return ir.Expression{}, false
			}
			entries = append(entries, ir.MapEntry{Key: key, Value: value})
		}
		return ir.Expression{Kind: ir.ExprMap, Type: l.hirType(typ), Entries: entries}, true
	case "struct":
		literalFields := l.structLiteralFields(expr.Type, typ)
		if structured {
			literalFields = l.semanticStructLiteralFields(expr, composite)
		}
		unkeyed := len(expr.Items) != 0 && expr.Items[0].Key == nil
		for _, item := range expr.Items {
			if (item.Key == nil) != unkeyed {
				l.add("hirgen.composite.struct.mixed", "struct composite literal cannot mix keyed and unkeyed elements", expr.Span)
				return ir.Expression{}, false
			}
		}
		if unkeyed {
			if len(literalFields) != len(expr.Items) {
				l.add("hirgen.composite.struct.field_count", "struct composite element count must match field count", expr.Span)
				return ir.Expression{}, false
			}
			values := make([]ir.FieldValue, 0, len(expr.Items))
			for i, item := range expr.Items {
				element := item.Value
				field := literalFields[i]
				if field.name == "" {
					l.add("hirgen.composite.struct.field", "struct composite element requires a named field", field.span)
					return ir.Expression{}, false
				}
				if field.external && !isExported(field.name) {
					l.add("hirgen.composite.struct.unexported", "cannot specify unexported field from another module", field.span)
					return ir.Expression{}, false
				}
				value, ok := l.lowerCompositeValueInType(element, field.typ, scope)
				if !ok {
					return ir.Expression{}, false
				}
				if hirResultCount(value) != 1 {
					l.add("hirgen.composite.struct.value", "struct composite element must produce exactly one value", element.Span)
					return ir.Expression{}, false
				}
				if field.name == "_" {
					continue
				}
				values = append(values, ir.FieldValue{Name: field.name, Value: value})
			}
			return ir.Expression{Kind: ir.ExprStruct, Type: l.hirType(typ), Fields: values}, true
		}
		entries := expr.Items
		if len(entries) == 0 {
			fields := l.zeroStructFieldValues(literalFields)
			return ir.Expression{Kind: ir.ExprStruct, Type: l.hirType(typ), Fields: fields}, true
		}
		fields := make([]ir.FieldValue, 0, len(entries))
		seen := map[string]struct{}{}
		for _, entry := range entries {
			if entry.Key == nil || entry.Key.Kind != ast.ExprIdent || strings.TrimSpace(entry.Key.Name) == "" {
				l.add("hirgen.composite.struct.field", "struct composite entry requires an identifier field key", expr.Span)
				return ir.Expression{}, false
			}
			fieldName := strings.TrimSpace(entry.Key.Name)
			if fieldName == "_" {
				l.add("hirgen.composite.struct.unknown", "blank field cannot be specified in a keyed struct literal", entry.Key.Span)
				return ir.Expression{}, false
			}
			if _, duplicate := seen[fieldName]; duplicate {
				l.add("hirgen.composite.struct.duplicate", "duplicate struct composite field", entry.Key.Span)
				return ir.Expression{}, false
			}
			seen[fieldName] = struct{}{}
			field, ok := structLiteralFieldByName(literalFields, fieldName)
			if !ok {
				l.add("hirgen.composite.struct.unknown", "struct composite field is not declared in literal type", entry.Key.Span)
				return ir.Expression{}, false
			}
			if field.external && !isExported(field.name) {
				l.add("hirgen.composite.struct.unexported", "cannot specify unexported field from another module", entry.Key.Span)
				return ir.Expression{}, false
			}
			value, ok := l.lowerCompositeValueInType(entry.Value, field.typ, scope)
			if !ok {
				return ir.Expression{}, false
			}
			fields = append(fields, ir.FieldValue{Name: field.name, Value: value})
		}
		return ir.Expression{Kind: ir.ExprStruct, Type: l.hirType(typ), Fields: fields}, true
	default:
		l.add("hirgen.composite.type", "unsupported composite literal type", expr.Span)
		return ir.Expression{}, false
	}
}

func (l *lowerer) semanticStructLiteralFields(expr ast.Expression, info check.CompositeInfo) []structLiteralField {
	out := make([]structLiteralField, 0, len(info.Fields))
	external := strings.TrimSpace(info.ModulePath) != "" && strings.TrimSpace(info.ModulePath) != l.modulePath
	declared := l.structDeclFields(expr.Type)
	for i, field := range info.Fields {
		variadic := field.Variadic
		span := expr.Span
		if i < len(declared) {
			span = declared[i].Span
		}
		out = append(out, structLiteralField{
			name: strings.TrimSpace(field.Name), typ: l.formatSemanticType(field.Type),
			variadic: &variadic, span: span, external: external,
		})
	}
	return out
}

type structLiteralField struct {
	name     string
	typ      string
	variadic *bool
	span     source.Span
	external bool
}

func (l *lowerer) structLiteralFields(typ *ast.TypeExpr, canonical string) []structLiteralField {
	if typ != nil && (typ.Kind == ast.TypeStruct || typeString(*typ) != "") {
		fields := l.structDeclFields(typ)
		if typ.Kind == ast.TypeStruct || len(fields) != 0 {
			out := make([]structLiteralField, 0, len(fields))
			for _, field := range fields {
				var variadic *bool
				if field.Type.Kind == ast.TypeFunc {
					value := astFunctionTypeVariadic(field.Type)
					variadic = &value
				}
				out = append(out, structLiteralField{
					name:     l.structFieldName(field),
					typ:      l.resolveSourceType(field.Type),
					variadic: variadic,
					span:     field.Span,
				})
			}
			return out
		}
	}
	if export, ok := l.importedTypeInfo(canonical); ok {
		out := make([]structLiteralField, 0, len(export.Fields))
		external := strings.TrimSpace(export.ModulePath) != "" && strings.TrimSpace(export.ModulePath) != l.modulePath
		for _, field := range export.Fields {
			variadic := field.Variadic
			out = append(out, structLiteralField{
				name:     strings.TrimSpace(field.Name),
				typ:      l.resolveType(field.Type),
				variadic: &variadic,
				external: external,
			})
		}
		return out
	}
	view, ok := l.typeView(l.resolveNamedUnderlyingType(canonical))
	if !ok || view.Shape() != types.Struct {
		return nil
	}
	fields, ok := view.StructFields()
	if !ok {
		return nil
	}
	out := make([]structLiteralField, 0, len(fields))
	for _, field := range fields {
		out = append(out, structLiteralField{
			name: strings.TrimSpace(field.Name),
			typ:  l.resolveType(l.typeRefString(field.Type)),
		})
	}
	return out
}

func (l *lowerer) zeroStructFieldValues(fields []structLiteralField) []ir.FieldValue {
	values := make([]ir.FieldValue, 0, len(fields))
	for _, field := range fields {
		if field.name == "" || field.name == "_" {
			continue
		}
		values = append(values, ir.FieldValue{
			Name:  field.name,
			Value: ir.Expression{Kind: ir.ExprZero, Type: l.hirType(field.typ)},
		})
	}
	return values
}

func structLiteralFieldByName(fields []structLiteralField, name string) (structLiteralField, bool) {
	for _, field := range fields {
		if field.name == name {
			return field, true
		}
	}
	return structLiteralField{}, false
}

func (l *lowerer) structDeclFields(typ *ast.TypeExpr) []ast.Field {
	if typ == nil {
		return nil
	}
	structType := *typ
	if typ.Kind != ast.TypeStruct {
		if decl, ok := l.typeDeclFor(l.resolveSourceType(*typ)); ok {
			structType = decl
		}
	}
	if structType.Kind != ast.TypeStruct {
		return nil
	}
	return structType.Fields
}

type arrayAssignment struct {
	index int64
	value ast.Expression
}

func (l *lowerer) lowerArrayCompositeItems(expr ast.Expression, scope *funcScope, typ string) (ir.Expression, bool) {
	assignments := make([]arrayAssignment, 0, len(expr.Items))
	seen := map[int64]struct{}{}
	nextIndex := int64(0)
	maxIndex := int64(-1)
	for _, item := range expr.Items {
		index := nextIndex
		if item.Key != nil {
			key, ok := l.arrayCompositeKeyIndex(*item.Key, scope)
			if !ok {
				l.add("hirgen.composite.array.key", "array composite key must be a non-negative integer constant", item.Key.Span)
				return ir.Expression{}, false
			}
			index = key
		}
		if _, exists := seen[index]; exists {
			l.add("hirgen.composite.array.duplicate", "duplicate array composite index", item.Value.Span)
			return ir.Expression{}, false
		}
		seen[index] = struct{}{}
		assignments = append(assignments, arrayAssignment{index: index, value: item.Value})
		if index > maxIndex {
			maxIndex = index
		}
		nextIndex = index + 1
	}
	return l.lowerArrayAssignments(expr, scope, typ, assignments, maxIndex)
}

func (l *lowerer) lowerArrayAssignments(expr ast.Expression, scope *funcScope, typ string, assignments []arrayAssignment, maxIndex int64) (ir.Expression, bool) {
	length := maxIndex + 1
	elemType := ""
	if expr.Type != nil && expr.Type.Kind == ast.TypeArray && expr.Type.LenInfer {
		if expr.Type.Elem != nil {
			elemType = l.resolveSourceTypeInScope(*expr.Type.Elem, scope)
		}
		if elemType == "" {
			elemType = "Any"
		}
		if length < 0 {
			length = 0
		}
	} else {
		if declared, present, ok := l.arrayCompositeLength(expr, typ, scope); !ok {
			l.add("hirgen.composite.array.length", "array composite length must be a non-negative integer literal", expr.Span)
			return ir.Expression{}, false
		} else if present {
			if maxIndex >= declared {
				l.add("hirgen.composite.array.index", "array composite index exceeds declared length", expr.Span)
				return ir.Expression{}, false
			}
			length = declared
		}
	}
	maxInt := int64(int(^uint(0) >> 1))
	if length > maxInt {
		l.add("hirgen.composite.array.length", "array composite length is too large", expr.Span)
		return ir.Expression{}, false
	}
	if expr.Type != nil && expr.Type.Kind == ast.TypeArray && expr.Type.LenInfer {
		typ = arrayTypeString(length, elemType)
	} else {
		if composite, ok := l.semanticCompositeFact(expr); ok {
			elemType = l.formatSemanticType(composite.Element)
		}
		if elemType == "" {
			elemType = l.indexElementType(typ)
		}
	}
	if elemType == "" {
		elemType = "Any"
	}
	elements := make([]ir.Expression, int(length))
	for i := range elements {
		elements[i] = ir.Expression{Kind: ir.ExprZero, Type: l.hirType(elemType)}
	}
	for _, assignment := range assignments {
		value, ok := l.lowerCompositeValueInType(assignment.value, elemType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		elements[int(assignment.index)] = value
	}
	return ir.Expression{Kind: ir.ExprSequence, Type: l.hirType(typ), Elements: elements}, true
}

func (l *lowerer) arrayCompositeKeyIndex(expr ast.Expression, scope *funcScope) (int64, bool) {
	raw, typ, ok := l.constValue(expr, scope)
	if !ok {
		return 0, false
	}
	converted, convertedType, ok := l.convertConstValue(raw, typ, "Int")
	if !ok {
		return 0, false
	}
	index, ok := l.constInt64(converted, convertedType)
	if !ok || index < 0 {
		return 0, false
	}
	return index, true
}

func (l *lowerer) arrayCompositeLength(expr ast.Expression, typ string, scope *funcScope) (int64, bool, bool) {
	declared, present, ok := l.arrayCompositeDeclaredLength(expr.Type, scope)
	if !ok || present {
		return declared, present, ok
	}
	lengthValue, _, ok := l.arrayTypeInfo(typ)
	if !ok {
		return 0, false, true
	}
	if lengthValue < 0 {
		return 0, true, false
	}
	return lengthValue, true, true
}

func (l *lowerer) arrayCompositeDeclaredLength(typ *ast.TypeExpr, scope *funcScope) (int64, bool, bool) {
	if typ == nil || typ.Kind != ast.TypeArray {
		return 0, false, true
	}
	if typ.Len == nil {
		return 0, false, true
	}
	length, ok := l.arrayLengthConst(typ.Len, scope)
	if !ok {
		return 0, true, false
	}
	return length, true, true
}

func (l *lowerer) lowerOperand(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	if expr.Operand == nil {
		l.add("hirgen.operand.missing", "missing operand", expr.Span)
		return ir.Expression{}, false
	}
	return l.lowerExpression(*expr.Operand, scope)
}

func (l *lowerer) lowerExpressions(expressions []ast.Expression, scope *funcScope) ([]ir.Expression, bool) {
	out := make([]ir.Expression, 0, len(expressions))
	for _, expr := range expressions {
		lowered, ok := l.lowerExpression(expr, scope)
		if !ok {
			return nil, false
		}
		out = append(out, lowered)
	}
	return out, true
}

func (l *lowerer) lowerExpressionsInType(expressions []ast.Expression, targetType string, scope *funcScope) ([]ir.Expression, bool) {
	out := make([]ir.Expression, 0, len(expressions))
	for _, expr := range expressions {
		lowered, ok := l.lowerExpressionInType(expr, targetType, scope)
		if !ok {
			return nil, false
		}
		out = append(out, lowered)
	}
	return out, true
}
