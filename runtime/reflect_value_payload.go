package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func reflectDynamicValue(ctx intrinsicContext, value vmValue) (vmValue, bool, error) {
	module := reflectRelationModule(ctx)
	for {
		switch {
		case value.Type.Ref.Kind == types.Any:
			payload := value.payload()
			if payload == nil {
				return vmValue{}, false, nil
			}
			inner, ok := payload.(vmValue)
			if !ok {
				return vmValue{}, false, errors.New("reflect.value_of invalid Any value")
			}
			value = inner
		case module != nil && module.isInterfaceType(value.Type):
			payload := value.payload()
			if payload == nil {
				return vmValue{}, false, nil
			}
			inner, ok := payload.(vmValue)
			if !ok {
				return vmValue{}, false, errors.New("reflect.value_of invalid interface value")
			}
			value = inner
		default:
			return value, true, nil
		}
	}
}

type reflectValueFields struct {
	value *vmStruct
}

func (fields reflectValueFields) lookup(name string) (vmValue, bool) {
	return structValueField(fields.value, name)
}

func (fields reflectValueFields) get(name string) vmValue {
	value, _ := fields.lookup(name)
	return value
}

func reflectValuePayload(value vmValue) (reflectValueFields, error) {
	if !isReflectValueTypeName(value.Type.String()) {
		return reflectValueFields{}, fmt.Errorf("reflect: expected Value, got %s", value.Type)
	}
	payload, ok := value.payload().(*vmStruct)
	if !ok || payload == nil || payload.schema == nil {
		return reflectValueFields{}, errors.New("reflect: invalid Value payload")
	}
	fields := reflectValueFields{value: payload}
	// Typed storage fixes field types at publication. Validate its schema
	// without decoding unrelated strings, dynamic values or slice headers.
	fieldType := func(name string) (vmType, bool) {
		if payload.inlineRaw != nil || payload.managedRef.valid() {
			_, field, exists := payload.schema.field(name)
			return field.RuntimeType, exists
		}
		field, exists := fields.lookup(name)
		return field.Type, exists
	}
	for _, field := range []struct {
		name      string
		primitive types.PrimitiveKind
	}{
		{"valid", types.PrimitiveBool},
		{"addressable", types.PrimitiveBool},
		{"settable", types.PrimitiveBool},
		{"interfaceable", types.PrimitiveBool},
		{"embeddedReadOnly", types.PrimitiveBool},
		{"zero", types.PrimitiveBool},
		{"nilValue", types.PrimitiveBool},
		{"length", types.PrimitiveInt},
		{"capacity", types.PrimitiveInt},
		{"boolValue", types.PrimitiveBool},
		{"intValue", types.PrimitiveInt64},
		{"uintValue", types.PrimitiveUint64},
		{"floatValue", types.PrimitiveFloat64},
		{"complexValue", types.PrimitiveComplex128},
		{"stringValue", types.PrimitiveString},
	} {
		item, exists := fieldType(field.name)
		if exists && !item.Primitive(field.primitive) {
			return reflectValueFields{}, fmt.Errorf("reflect: Value payload field %s has invalid type", field.name)
		}
	}
	data, exists := fieldType("data")
	if !exists || data.Ref.Kind != types.Any {
		return reflectValueFields{}, errors.New("reflect: Value payload field data has invalid type")
	}
	if target, exists := fieldType("target"); exists && target.Ref.Kind != types.Any {
		return reflectValueFields{}, errors.New("reflect: Value payload field target has invalid type")
	}
	if typ, exists := fieldType("valueType"); !exists || (!typ.Is(types.Interface) && typ.String() != "reflect.Type") {
		return reflectValueFields{}, errors.New("reflect: Value payload field valueType has invalid type")
	}
	if entries, exists := fieldType("mapEntries"); exists && !entries.Is(types.Slice) {
		return reflectValueFields{}, errors.New("reflect: Value payload field mapEntries has invalid type")
	}
	return fields, nil
}

func isReflectValueTypeName(typ string) bool {
	typ = strings.TrimSpace(typ)
	return typ == "Value" || typ == "reflect.Value"
}

func reflectAnyPayload(value vmValue) (vmValue, bool) {
	if value.Type.Ref.Kind != types.Any {
		return vmValue{}, false
	}
	inner, ok := value.payload().(vmValue)
	if !ok {
		return vmValue{}, false
	}
	return inner, true
}

func reflectCurrentValue(fields reflectValueFields) (vmValue, bool, error) {
	if target, ok, err := reflectTargetPayload(fields); ok || err != nil {
		if err != nil {
			return vmValue{}, false, err
		}
		current, err := derefPointer(target)
		if err != nil {
			return vmValue{}, false, err
		}
		return current, true, nil
	}
	value, ok := reflectAnyPayload(fields.get("data"))
	// Dynamic slots store the concrete interface payload. A reflected Value
	// also has a declared type, which can itself be an interface (for example
	// a struct field). Reconstruct that view from its existing type descriptor
	// instead of treating the concrete value as the field's declared type.
	if key, err := reflectTypeKeyFromValue(fields.get("valueType")); err == nil {
		typ := runtimeTypeFromText(key)
		if fields.value.owner != nil {
			typ = fields.value.owner.rootModule().resolvedRuntimeType(typ)
		}
		if typ.Ref.Kind == types.Any || typ.ShapeKind() == types.Interface {
			if !ok {
				return newVMValue(typ, nil), true, nil
			}
			if !value.Type.Equal(typ) {
				value = newVMValue(typ, value)
			}
		}
	}
	return value, ok, nil
}

func reflectTargetPayload(fields reflectValueFields) (vmValue, bool, error) {
	target, ok := reflectAnyPayload(fields.get("target"))
	if !ok {
		return vmValue{}, false, nil
	}
	if _, ok := target.Type.PointerElem(); !ok {
		return vmValue{}, false, fmt.Errorf("reflect: invalid Value target %s", target.Type)
	}
	return target, true, nil
}

func reflectIndexPath(value vmValue) ([]int, error) {
	items, ok := sliceValues(value)
	if !ok {
		return nil, errors.New("reflect: field index must be []int")
	}
	out := make([]int, 0, len(items))
	for i, item := range items {
		index, err := asInt64(item)
		if err != nil {
			return nil, fmt.Errorf("reflect: field index %d: %w", i, err)
		}
		if index < 0 {
			return nil, fmt.Errorf("reflect: negative field index %d", index)
		}
		out = append(out, int(index))
	}
	if len(out) == 0 {
		return nil, errors.New("reflect: empty field index")
	}
	return out, nil
}

func reflectFieldPointerByIndex(module *moduleInstance, rootPtr vmValue, index []int, rootFields []TypeFieldInfo, interfaceable bool) (vmValue, bool, bool, error) {
	currentPtr := rootPtr
	fields := rootFields
	fieldInterfaceable := interfaceable
	embeddedReadOnly := false
	for pathIndex, fieldIndex := range index {
		current, err := derefPointer(currentPtr)
		if err != nil {
			return vmValue{}, false, false, err
		}
		if len(fields) == 0 {
			info := reflectTypeInfoForValue(module, current)
			if strings.TrimSpace(info.Kind) != "struct" {
				return vmValue{}, false, false, errors.New("reflect: non-struct field in index path")
			}
			fields = info.Fields
		}
		if fieldIndex < 0 || fieldIndex >= len(fields) {
			return vmValue{}, false, false, fmt.Errorf("reflect: field index out of range: %d", fieldIndex)
		}
		field := fields[fieldIndex]
		fieldInterfaceable = interfaceable && field.Exported
		embeddedReadOnly = interfaceable && !field.Exported && field.Embedded
		if !field.Exported && !field.Embedded {
			interfaceable = false
		}
		currentPtr = reflectStructFieldPointer(module, currentPtr, field)
		if pathIndex+1 == len(index) {
			break
		}
		if nested, ok := module.moduleStructFieldInfo(typeFieldRuntimeType(field)); ok {
			fields = nested
		} else {
			fields = nil
		}
	}
	return currentPtr, fieldInterfaceable, embeddedReadOnly, nil
}

func reflectTypeKeyFromValue(value vmValue) (string, error) {
	payload := value.payload()
	if payload == nil {
		return "", errors.New("reflect: nil Type")
	}
	if inner, ok := payload.(vmValue); ok {
		payload = inner.payload()
	}
	if !isStructValue(payload) {
		return "", errors.New("reflect: invalid Type payload")
	}
	key, ok := structValueField(payload, "key")
	if !ok || !key.Type.Primitive(types.PrimitiveString) {
		return "", errors.New("reflect: Type payload is missing its canonical key")
	}
	text, ok := key.payload().(string)
	text = strings.TrimSpace(text)
	if !ok || text == "" {
		return "", errors.New("reflect: Type payload has an invalid canonical key")
	}
	return text, nil
}

func reflectMethodInfoFromValue(ctx intrinsicContext, value vmValue) (TypeMethodInfo, error) {
	if strings.TrimSpace(value.Type.String()) != "Method" && strings.TrimSpace(value.Type.String()) != "reflect.Method" {
		return TypeMethodInfo{}, fmt.Errorf("reflect: expected Method, got %s", value.Type)
	}
	fields, ok := materializeStructValue(value.payload())
	if !ok {
		return TypeMethodInfo{}, errors.New("reflect: invalid Method payload")
	}
	stringsByName := make(map[string]string, 4)
	for _, name := range []string{"modulePath", "Name", "PkgPath", "functionID"} {
		field, exists := fields[name]
		text, valid := field.payload().(string)
		if !exists || !field.Type.Primitive(types.PrimitiveString) || !valid {
			return TypeMethodInfo{}, fmt.Errorf("reflect: Method payload field %s has invalid type", name)
		}
		stringsByName[name] = text
	}
	receiverInfo, err := reflectResolvedTypeInfo(ctx, fields["receiverType"])
	if err != nil {
		return TypeMethodInfo{}, fmt.Errorf("reflect: Method payload field receiverType: %w", err)
	}
	signatureInfo, err := reflectResolvedTypeInfo(ctx, fields["signatureType"])
	if err != nil {
		return TypeMethodInfo{}, fmt.Errorf("reflect: Method payload field signatureType: %w", err)
	}
	module := reflectRelationModule(ctx)
	if module == nil {
		return TypeMethodInfo{}, errors.New("reflect: Method payload requires module context")
	}
	variadic, variadicOK := fields["variadic"].boolValue()
	exported, exportedOK := fields["exported"].boolValue()
	if !variadicOK || !fields["variadic"].Type.Primitive(types.PrimitiveBool) ||
		!exportedOK || !fields["exported"].Type.Primitive(types.PrimitiveBool) {
		return TypeMethodInfo{}, errors.New("reflect: Method payload flags have invalid type")
	}
	return TypeMethodInfo{
		ModulePath: stringsByName["modulePath"], Name: stringsByName["Name"],
		PkgPath:       stringsByName["PkgPath"],
		ReceiverType:  module.resolvedRuntimeType(receiverInfo.Key),
		SignatureType: module.resolvedRuntimeType(signatureInfo.Key),
		FunctionID:    stringsByName["functionID"], Variadic: variadic, Exported: exported,
	}, nil
}

func reflectBoolField(value vmValue) bool {
	flag, _ := value.boolValue()
	return flag
}

func reflectStructFieldPointer(module *moduleInstance, parentPtr vmValue, field TypeFieldInfo) vmValue {
	fieldName := strings.TrimSpace(field.Name)
	fieldType := typeFieldRuntimeType(field)
	identity := "reflect-field:" + fieldName
	if pointer, err := pointerValue(parentPtr); err == nil && strings.TrimSpace(pointer.Identity) != "" {
		identity = pointer.Identity + ".field:" + fieldName
	}
	return newTargetPointer(&vmPointer{Type: coerceRuntimeType(fieldType), Identity: identity, target: pointerField, module: module, parent: parentPtr, field: fieldName})
}

func reflectIndexPointer(module *moduleInstance, parentPtr vmValue, index int64) (vmValue, error) {
	parent, err := derefPointer(parentPtr)
	if err != nil {
		return vmValue{}, err
	}
	if index < 0 {
		return vmValue{}, fmt.Errorf("reflect: negative index %d", index)
	}
	elemType := module.sequenceElementType(parent.Type)
	if elemType == "" {
		return vmValue{}, errors.New("reflect: Index of non-array/slice Value")
	}
	indexArg := newVMValue("Int", index)
	if _, err := indexValue(module, parent, indexArg); err != nil {
		return vmValue{}, err
	}
	if _, ok := parent.payload().(*vmArray); ok {
		// A zero array field may still be implicit in its enclosing struct.
		// Attach it before capturing a stable element address.
		if err := commitPointerMutation(parentPtr, parent); err != nil {
			return vmValue{}, err
		}
		parent, err = derefPointer(parentPtr)
		if err != nil {
			return vmValue{}, err
		}
	}
	identity := fmt.Sprintf("reflect-index:%d", index)
	if pointer, err := pointerValue(parentPtr); err == nil && strings.TrimSpace(pointer.Identity) != "" {
		identity = fmt.Sprintf("%s.index:%d", pointer.Identity, index)
	}
	var slice *vmSlice
	switch data := parent.payload().(type) {
	case *vmSlice:
		slice = data
	case *vmArray:
		slice = &data.vmSlice
	}
	if slice != nil {
		identity = slice.elementIdentity(index)
		parentPtr = parent
	}
	return newTargetPointer(&vmPointer{Type: coerceRuntimeType(elemType), Identity: identity, target: pointerIndex, module: module, parent: parentPtr, index: index}), nil
}
