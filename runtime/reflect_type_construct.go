package runtime

import (
	"errors"
	"fmt"
	"go/token"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func reflectArrayOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.array_of expects 2 arguments, got %d", len(args))
	}
	length, err := asInt64(args[0])
	if err != nil || length < 0 {
		return reflectTypeError("reflect.ArrayOf: negative length"), nil
	}
	elem, err := reflectRuntimeTypeFromTypeValue(ctx, args[1])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	return reflectRegisterDynamicType(ctx, fmt.Sprintf("Array<%d,%s>", length, elem), TypeInfo{})
}

func reflectChanOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.chan_of expects 2 arguments, got %d", len(args))
	}
	direction, err := asInt64(args[0])
	if err != nil || direction < 1 || direction > 3 {
		return reflectTypeError("reflect.ChanOf: invalid direction"), nil
	}
	elem, err := reflectRuntimeTypeFromTypeValue(ctx, args[1])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	prefix := "Waitable<"
	switch direction {
	case 1:
		prefix = "ReceiveWaitable<"
	case 2:
		prefix = "SendWaitable<"
	}
	return reflectRegisterDynamicType(ctx, prefix+elem+">", TypeInfo{})
}

func reflectFuncOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("reflect.func_of expects 3 arguments, got %d", len(args))
	}
	inputs, err := reflectTypeValueSlice(ctx, args[0])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	outputs, err := reflectTypeValueSlice(ctx, args[1])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	variadic, ok := args[2].Data.(bool)
	if !ok || !args[2].Type.Primitive(types.PrimitiveBool) {
		return reflectTypeError("reflect.FuncOf: variadic flag is not bool"), nil
	}
	if variadic {
		if len(inputs) == 0 || canonicalTypeKind(inputs[len(inputs)-1]) != "slice" {
			return reflectTypeError("reflect.FuncOf: last arg of variadic func must be slice"), nil
		}
		inputs[len(inputs)-1] = "variadic " + inputs[len(inputs)-1]
	}
	result := "Void"
	if len(outputs) == 1 {
		result = outputs[0]
	} else if len(outputs) > 1 {
		result = "tuple(" + strings.Join(outputs, ",") + ")"
	}
	signature := "function(" + strings.Join(inputs, ",") + ") " + result
	return reflectRegisterDynamicType(ctx, signature, TypeInfo{Variadic: variadic})
}

func reflectStructOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.struct_of expects 1 argument, got %d", len(args))
	}
	values, ok := sliceValues(args[0])
	if !ok {
		return reflectTypeError("reflect.StructOf: fields must be []StructField"), nil
	}
	module := reflectRelationModule(ctx)
	fields := make([]TypeFieldInfo, 0, len(values))
	canonical := make([]string, 0, len(values))
	names := make(map[string]bool, len(values))
	offset := uint64(0)
	maxAlign := 1
	for index, value := range values {
		data, ok := materializeStructValue(value.Data)
		if !ok {
			return reflectTypeError(fmt.Sprintf("reflect.StructOf: invalid field %d", index)), nil
		}
		nameValue, nameExists := data["Name"]
		name, nameOK := nameValue.Data.(string)
		pkgPathValue, pkgPathExists := data["PkgPath"]
		pkgPath, pkgPathOK := pkgPathValue.Data.(string)
		tagValue, tagExists := data["Tag"]
		tag, tagOK := tagValue.Data.(string)
		anonymousValue, anonymousExists := data["Anonymous"]
		anonymous, anonymousOK := anonymousValue.Data.(bool)
		if !nameExists || !nameOK || !module.resolvedRuntimeType(nameValue.Type).Primitive(types.PrimitiveString) ||
			!pkgPathExists || !pkgPathOK || !module.resolvedRuntimeType(pkgPathValue.Type).Primitive(types.PrimitiveString) ||
			!tagExists || !tagOK || !module.resolvedRuntimeType(tagValue.Type).Primitive(types.PrimitiveString) ||
			!anonymousExists || !anonymousOK || !module.resolvedRuntimeType(anonymousValue.Type).Primitive(types.PrimitiveBool) {
			return reflectTypeError(fmt.Sprintf("reflect.StructOf: invalid field metadata at index %d", index)), nil
		}
		if !token.IsIdentifier(name) || name == "_" || names[name] {
			return reflectTypeError("reflect.StructOf: invalid or duplicate field " + name), nil
		}
		names[name] = true
		fieldType, err := reflectRuntimeTypeFromTypeValue(ctx, data["Type"])
		if err != nil {
			return reflectTypeError("reflect.StructOf: field " + name + " has no type"), nil
		}
		if strings.Contains(tag, "`") {
			return reflectTypeError("reflect.StructOf: field tag contains backquote"), nil
		}
		if pkgPath != "" || !isExportedName(name) {
			return reflectTypeError("reflect.StructOf: unexported fields are not supported"), nil
		}
		fieldInfo, err := reflectResolvedTypeInfo(ctx, data["Type"])
		if err != nil {
			return reflectTypeError("reflect.StructOf: field " + name + " has invalid type"), nil
		}
		layout := module.reflectTypeLayout(fieldType)
		if layout.align < 1 {
			layout.align = 1
		}
		offset = reflectAlignUp(offset, uint64(layout.align))
		field := TypeFieldInfo{
			Name: name, PkgPath: pkgPath,
			RuntimeType: module.resolvedRuntimeType(fieldInfo.Key), Tag: tag, Embedded: anonymous,
			Offset: offset, Exported: pkgPath == "",
		}
		fields = append(fields, field)
		part := formatCanonicalStructField(name, fieldType, tag, anonymous)
		canonical = append(canonical, part)
		offset += layout.size
		if layout.align > maxAlign {
			maxAlign = layout.align
		}
	}
	typ := "struct{" + strings.Join(canonical, ",") + "}"
	info := TypeInfo{
		Key: typ, Kind: "struct",
		Type: typ, QualifiedType: typ, Fields: fields,
		Align: maxAlign, FieldAlign: maxAlign, Size: reflectAlignUp(offset, uint64(maxAlign)),
	}
	return reflectRegisterDynamicType(ctx, typ, info)
}

func reflectTypeValueSlice(ctx intrinsicContext, value vmValue) ([]string, error) {
	items, ok := sliceValues(value)
	if !ok {
		return nil, errors.New("reflect: expected []Type")
	}
	out := make([]string, len(items))
	for i, item := range items {
		typ, err := reflectRuntimeTypeFromTypeValue(ctx, item)
		if err != nil {
			return nil, fmt.Errorf("reflect: type %d is nil", i)
		}
		out[i] = typ
	}
	return out, nil
}

func reflectRegisterDynamicType(ctx intrinsicContext, typ string, overrides TypeInfo) ([]vmValue, error) {
	// Constructor inputs already carry qualified element identities. Normalize
	// spelling without populating a revision's type caches before the budget check.
	typ = coerceRuntimeType(typ).String()
	module := reflectRelationModule(ctx)
	if ctx.vm != nil {
		if info, ok := ctx.vm.reflectTypes.load(typ); ok {
			return reflectTypeOK(reflectTypeValue(module, info)), nil
		}
	}
	// Include type parsing/layout metadata as logical storage, not actual Go heap.
	size := int64(len(typ))*64 + 512 + int64(len(overrides.Fields))*128
	if ctx.vm != nil {
		limits := normalizeLimits(ctx.vm.limits)
		if ctx.vm.dynamicTypeCount >= limits.MaxDynamicTypes {
			return nil, ResourceLimitError{Code: "execution.dynamic_type_limit", Message: fmt.Sprintf("dynamic reflection type limit exceeded: max %d", limits.MaxDynamicTypes)}
		}
		if size > limits.MaxDynamicTypeBytes-ctx.vm.dynamicTypeBytes {
			return nil, ResourceLimitError{Code: "execution.dynamic_type_bytes_limit", Message: fmt.Sprintf("dynamic reflection metadata byte limit exceeded: max %d", limits.MaxDynamicTypeBytes)}
		}
		if err := ctx.vm.chargeAllocationBytes(size); err != nil {
			return nil, err
		}
	}
	info := reflectTypeInfoForTypeText(module, typ)
	info.Key = typ
	if overrides.Kind != "" {
		info.Kind = overrides.Kind
	}
	if overrides.Type != "" {
		info.Type = overrides.Type
	}
	if overrides.QualifiedType != "" {
		info.QualifiedType = overrides.QualifiedType
	}
	if overrides.Display != "" {
		info.Display = overrides.Display
	}
	if overrides.Fields != nil {
		info.Fields = overrides.Fields
	}
	if overrides.Variadic {
		info.Variadic = true
	}
	if overrides.Align != 0 {
		info.Align, info.FieldAlign, info.Size = overrides.Align, overrides.FieldAlign, overrides.Size
	}
	if info.Display == "" {
		info.Display = reflectSourceType(module, module.resolvedRuntimeType(typ))
	}
	if ctx.vm != nil {
		ctx.vm.reflectTypes.store(info.Key, info)
		ctx.vm.dynamicTypeCount++
		ctx.vm.dynamicTypeBytes += size
	}
	return reflectTypeOK(reflectTypeValue(module, info)), nil
}
