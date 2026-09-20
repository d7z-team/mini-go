package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func reflectRuntimeTypeFromTypeValue(ctx intrinsicContext, value vmValue) (string, error) {
	info, err := reflectResolvedTypeInfo(ctx, value)
	if err != nil {
		return "", err
	}
	typ := strings.TrimSpace(info.Key)
	if typ == "" {
		return "", errors.New("reflect: empty Type")
	}
	return typ, nil
}

func reflectCellPointer(module *moduleInstance, typ, identity string, value vmValue) vmValue {
	cell := newSlot(coerceRuntimeType(typ), module, false)
	cell.publish(value)
	return newTargetPointer(&vmPointer{Type: coerceRuntimeType(typ), Identity: identity, target: pointerCell, module: module, cell: cell})
}

func reflectZeroValue(module *moduleInstance, typ string) vmValue {
	typ = strings.TrimSpace(typ)
	if module != nil {
		if target, name, ok := module.qualifiedTypeModule(typ); ok {
			value := target.zeroValue(name)
			return target.qualifyValueForExport(value)
		}
		return module.zeroValue(typ)
	}
	return zeroVMValue(typ)
}

func reflectRelationModule(ctx intrinsicContext) *moduleInstance {
	base := ctx.module
	if base == nil && ctx.vm != nil {
		base = ctx.vm.rootModule()
	}
	return base
}

func reflectTypeValueFromVM(vm *vm, info TypeInfo) vmValue {
	key := reflectTypeKey(info)
	info.Key = key
	if vm != nil {
		vm.reflectTypes.store(key, info)
		if value, ok := vm.reflectTypeValues.load(key); ok {
			return value
		}
	}
	var module *moduleInstance
	if vm != nil {
		module = vm.rootModule()
	}
	value := reflectTypeValue(module, info)
	if vm != nil {
		value = vm.reflectTypeValues.loadOrStore(key, value)
	}
	return value
}

func reflectTypeRelationArgs(ctx intrinsicContext, routeID string, args []vmValue) (vmType, vmType, error) {
	if len(args) != 2 {
		return vmType{}, vmType{}, fmt.Errorf("%s expects 2 arguments, got %d", routeID, len(args))
	}
	module := reflectRelationModule(ctx)
	if module == nil {
		return vmType{}, vmType{}, errors.New("reflect: type relation requires VM context")
	}
	source, err := reflectResolvedTypeInfo(ctx, args[0])
	if err != nil {
		return vmType{}, vmType{}, err
	}
	target, err := reflectResolvedTypeInfo(ctx, args[1])
	if err != nil {
		return vmType{}, vmType{}, err
	}
	sourceType := module.resolvedRuntimeType(source.Key)
	targetType := module.resolvedRuntimeType(target.Key)
	if !sourceType.Valid() || !targetType.Valid() {
		return vmType{}, vmType{}, fmt.Errorf("%s received an unavailable Type", routeID)
	}
	return sourceType, targetType, nil
}

func reflectTypeValue(module *moduleInstance, info TypeInfo) vmValue {
	key := reflectTypeKey(info)
	value := newRuntimeStructValue(module, "reflect.runtimeType", map[string]vmValue{
		"key": newVMValue("String", key),
	})
	return newVMValue("reflect.Type", value)
}

func reflectTypeKey(info TypeInfo) string {
	return strings.TrimSpace(info.Key)
}

func reflectTypeDescriptorValue(ctx intrinsicContext, info TypeInfo) vmValue {
	module := reflectRelationModule(ctx)
	cacheKey := reflectTypeKey(info)
	if module != nil {
		if descriptor, ok := module.reflectTypeDescriptorCache.load(cacheKey); ok {
			return descriptor
		}
	}
	runtimeType := vmType{}
	if module != nil {
		runtimeType = module.resolvedRuntimeType(info.Key)
	}
	underlying := runtimeType.Underlying()
	elem := zeroReflectTypeValue()
	key := zeroReflectTypeValue()
	length := int64(0)
	inputs := []vmValue{}
	outputs := []vmValue{}
	if arrayLength, child, ok := underlying.ArrayInfo(); ok {
		elem = reflectTypeValueForRuntimeType(ctx, module, child)
		length = int64(arrayLength)
	} else if child, ok := underlying.PointerElem(); ok {
		elem = reflectTypeValueForRuntimeType(ctx, module, child)
	} else if child, ok := underlying.SliceElem(); ok {
		elem = reflectTypeValueForRuntimeType(ctx, module, child)
	} else if waitable, ok := underlying.WaitableInfo(); ok {
		elem = reflectTypeValueForRuntimeType(ctx, module, waitable.Elem)
	} else if mapKey, mapElem, ok := underlying.MapInfo(); ok {
		key = reflectTypeValueForRuntimeType(ctx, module, mapKey)
		elem = reflectTypeValueForRuntimeType(ctx, module, mapElem)
	}
	if function, ok := underlying.FunctionInfo(); ok {
		inputs = make([]vmValue, len(function.Signature.Params))
		for index, param := range function.Signature.Params {
			inputs[index] = reflectTypeValueForRuntimeType(ctx, module, vmType{Ref: param.Type, Table: underlying.Table})
		}
		outputs = make([]vmValue, len(function.Signature.Results))
		for index, result := range function.Signature.Results {
			outputs[index] = reflectTypeValueForRuntimeType(ctx, module, vmType{Ref: result, Table: underlying.Table})
		}
	}
	comparable := false
	if module != nil {
		var err error
		comparable, err = module.isComparableRuntimeType(runtimeType, map[string]struct{}{})
		if err != nil {
			comparable = false
		}
	}
	descriptor := newRuntimeStructValue(module, "reflect.runtimeTypeData", map[string]vmValue{
		"key":         newVMValue("String", reflectTypeKey(info)),
		"pkgPath":     newVMValue("String", info.PkgPath),
		"typeName":    newVMValue("String", reflectSourceTypeName(info)),
		"displayName": newVMValue("String", reflectTypeDisplay(info)),
		"variadic":    newVMValue("Bool", info.Variadic),
		"exported":    newVMValue("Bool", info.Exported),
		"align":       newVMValue("Int", int64(info.Align)),
		"fieldAlign":  newVMValue("Int", int64(info.FieldAlign)),
		"size":        newVMValue("Uintptr", info.Size),
		"bits":        newVMValue("Int", int64(info.Bits)),
		"chanDir":     newVMValue("reflect.ChanDir", int64(info.ChanDir)),
		"kind":        newVMValue("reflect.Kind", int64(reflectKindCode(underlying))),
		"elem":        elem,
		"keyType":     key,
		"length":      newVMValue("Int", length),
		"inputs":      newSliceValue("Slice<reflect.Type>", inputs),
		"outputs":     newSliceValue("Slice<reflect.Type>", outputs),
		"comparable":  newVMValue("Bool", comparable),
		"fieldCount":  newVMValue("Int", int64(len(info.Fields))),
		"methodCount": newVMValue("Int", int64(len(info.Methods))),
	})
	if module != nil {
		module.reflectTypeDescriptorCache.store(cacheKey, descriptor)
	}
	return descriptor
}

func reflectTypeValueForRuntimeType(ctx intrinsicContext, module *moduleInstance, typ vmType) vmValue {
	if !typ.Valid() {
		return zeroReflectTypeValue()
	}
	if typ.Ref.Kind == types.Named && typ.Ref.Named.ModulePath != "" && typ.Ref.Named.DeclID != "" && ctx.vm != nil {
		if info, ok := ctx.vm.findType(typ.Ref.Named.ModulePath, string(typ.Ref.Named.DeclID)); ok {
			return reflectTypeValueFromVM(ctx.vm, info)
		}
	}
	if module == nil {
		return zeroReflectTypeValue()
	}
	info := reflectTypeInfoForTypeText(module, typ.String())
	return reflectTypeValueFromVM(ctx.vm, info)
}

func reflectKindCode(typ vmType) int {
	ref := typ.Ref
	if ref.Kind == types.Any {
		return 20
	}
	if ref.Kind == types.Primitive {
		switch ref.Primitive {
		case types.PrimitiveBool:
			return 1
		case types.PrimitiveInt:
			return 2
		case types.PrimitiveInt8:
			return 3
		case types.PrimitiveInt16:
			return 4
		case types.PrimitiveInt32:
			return 5
		case types.PrimitiveInt64:
			return 6
		case types.PrimitiveUint:
			return 7
		case types.PrimitiveUint8:
			return 8
		case types.PrimitiveUint16:
			return 9
		case types.PrimitiveUint32:
			return 10
		case types.PrimitiveUint64:
			return 11
		case types.PrimitiveUintptr:
			return 12
		case types.PrimitiveFloat32:
			return 13
		case types.PrimitiveFloat64:
			return 14
		case types.PrimitiveComplex64:
			return 15
		case types.PrimitiveComplex128:
			return 16
		case types.PrimitiveString:
			return 24
		case types.PrimitiveError:
			return 20
		case types.PrimitiveFunction:
			return 19
		}
	}
	switch typ.ShapeKind() {
	case types.Array:
		return 17
	case types.Waitable:
		return 18
	case types.Function:
		return 19
	case types.Interface:
		return 20
	case types.Map:
		return 21
	case types.Pointer:
		return 22
	case types.Slice:
		return 23
	case types.Struct:
		return 25
	default:
		return 0
	}
}

func reflectSourceTypeName(info TypeInfo) string {
	if strings.TrimSpace(info.ModulePath) != "" && strings.TrimSpace(info.Name) != "" {
		return info.Name
	}
	if info.Kind == "primitive" {
		return reflectSourceType(nil, coerceRuntimeType(info.Key))
	}
	return info.Name
}

func reflectTypeOK(value vmValue) []vmValue {
	return []vmValue{value, newVMValue("String", ""), newVMValue("Bool", true)}
}

func reflectTypeError(message string) []vmValue {
	if strings.TrimSpace(message) == "" {
		message = "reflect: invalid Type"
	}
	return []vmValue{zeroReflectTypeValue(), newVMValue("String", message), newVMValue("Bool", false)}
}

func reflectTypeInfoForTypeText(module *moduleInstance, typ string) TypeInfo {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return TypeInfo{}
	}
	if module != nil && !strings.ContainsAny(typ, "<>{}(), ") {
		if owner, name, ok := module.qualifiedTypeModule(typ); ok {
			if decl, found := owner.executable.Types[name]; found {
				return owner.typeInfo(decl)
			}
		}
		if module.executable != nil {
			if decl, found := module.executable.Types[typ]; found {
				return module.typeInfo(decl)
			}
		}
	}
	qualified := typ
	if module != nil {
		qualified = module.qualifyLocalType(typ)
	}
	name := ""
	qualifiedName := ""
	if isPrimitiveTypeName(typ) {
		name = typ
		qualifiedName = typ
	} else if !strings.ContainsAny(qualified, "<>{}(), ") {
		qualifiedName = qualified
		name = qualified
		if dot := strings.LastIndex(qualified, "."); dot >= 0 && dot+1 < len(qualified) {
			name = qualified[dot+1:]
		}
	}
	info := TypeInfo{
		Key:           qualified,
		Name:          name,
		QualifiedName: qualifiedName,
		Kind:          canonicalTypeKind(qualified),
		Type:          typ,
		QualifiedType: qualified,
		Exported:      isExportedName(name),
	}
	if module != nil {
		runtimeType := module.resolvedRuntimeType(qualified)
		info.Display = reflectSourceType(module, runtimeType)
		if function, ok := runtimeType.FunctionInfo(); ok {
			info.Variadic = function.Signature.Variadic
		}
		info.Align, info.FieldAlign, info.Size, info.Bits, info.ChanDir = reflectTypeLayoutValues(module.reflectTypeLayout(typ))
	} else {
		info.Display = reflectSourceType(nil, coerceRuntimeType(qualified))
	}
	info.PkgPath = packagePathForType(info.ModulePath, info.Name)
	if module != nil && info.Kind == "struct" {
		info.Fields = reflectStructFieldsForValueType(module, typ)
	}
	return info
}

func zeroReflectTypeValue() vmValue {
	return newVMValue("reflect.Type", nil)
}

func zeroReflectValueValue() vmValue {
	return newRuntimeStructValue(nil, "reflect.Value", map[string]vmValue{
		"valid":         newVMValue("Bool", false),
		"valueType":     zeroReflectTypeValue(),
		"data":          newVMValue("Any", nil),
		"target":        newVMValue("Any", nil),
		"addressable":   newVMValue("Bool", false),
		"settable":      newVMValue("Bool", false),
		"interfaceable": newVMValue("Bool", false),
		"zero":          newVMValue("Bool", true),
		"nilValue":      newVMValue("Bool", true),
		"length":        newVMValue("Int", int64(0)),
		"capacity":      newVMValue("Int", int64(0)),
		"boolValue":     newVMValue("Bool", false),
		"intValue":      newVMValue("Int64", int64(0)),
		"uintValue":     newVMValue("Uint64", uint64(0)),
		"floatValue":    newVMValue("Float64", float64(0)),
		"complexValue":  newVMValue("Complex128", complex128(0)),
		"stringValue":   newVMValue("String", ""),
		"mapEntries":    newSliceValue("Slice<reflect.valueMapEntry>", nil),
	})
}
