package runtime

import (
	"errors"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func reflectValueSnapshot(ctx intrinsicContext, value vmValue) (vmValue, error) {
	return reflectValueSnapshotWithTarget(ctx, value, vmValue{}, false, false, true)
}

// Pointer traversal and refreshing a Value preserve the non-sticky restriction
// of an unexported embedded field; selecting a child field clears it.
func reflectCopyEmbeddedReadOnly(out vmValue, source reflectValueFields) vmValue {
	if reflectBoolField(source.get("embeddedReadOnly")) {
		setRuntimeStructField(out.payload().(*vmStruct), "embeddedReadOnly", newBoolValue(true))
	}
	return out
}

func reflectValueSnapshotWithTarget(ctx intrinsicContext, value, target vmValue, addressable, settable, interfaceable bool) (vmValue, error) {
	module := reflectRelationModule(ctx)
	info := reflectTypeInfoForValue(module, value)
	mapEntries, err := reflectValueMapEntries(ctx, module, value)
	if err != nil {
		return vmValue{}, err
	}
	boolValue, intValue, uintValue, floatValue, complexValue, stringValue := reflectScalarValues(value)
	out := newRuntimeStructValue(module, "reflect.Value", nil)
	fields, ok := out.payload().(*vmStruct)
	if !ok || fields == nil {
		return vmValue{}, errors.New("reflect: invalid Value schema")
	}
	setRuntimeStructField(fields, "valid", newBoolValue(true))
	setRuntimeStructField(fields, "valueType", reflectTypeValueFromVM(ctx.vm, info))
	setRuntimeStructField(fields, "data", newVMValue("Any", value))
	if target.Type.Valid() || target.payload() != nil {
		setRuntimeStructField(fields, "target", newVMValue("Any", target))
	}
	if addressable {
		setRuntimeStructField(fields, "addressable", newBoolValue(true))
	}
	if settable {
		setRuntimeStructField(fields, "settable", newBoolValue(true))
	}
	if interfaceable {
		setRuntimeStructField(fields, "interfaceable", newBoolValue(true))
	}
	if reflectValueIsZero(module, value) {
		setRuntimeStructField(fields, "zero", newBoolValue(true))
	}
	if reflectValueIsNil(value, info) {
		setRuntimeStructField(fields, "nilValue", newBoolValue(true))
	}
	if length := reflectValueLen(module, value); length != 0 {
		setRuntimeStructField(fields, "length", newVMValue("Int", length))
	}
	if capacity := reflectValueCap(module, value); capacity != 0 {
		setRuntimeStructField(fields, "capacity", newVMValue("Int", capacity))
	}
	if boolValue {
		setRuntimeStructField(fields, "boolValue", newBoolValue(true))
	}
	if intValue != 0 {
		setRuntimeStructField(fields, "intValue", newVMValue("Int64", intValue))
	}
	if uintValue != 0 {
		setRuntimeStructField(fields, "uintValue", newVMValue("Uint64", uintValue))
	}
	if floatValue != 0 {
		setRuntimeStructField(fields, "floatValue", newVMValue("Float64", floatValue))
	}
	if complexValue != 0 {
		setRuntimeStructField(fields, "complexValue", newVMValue("Complex128", complexValue))
	}
	if stringValue != "" {
		setRuntimeStructField(fields, "stringValue", newVMValue("String", stringValue))
	}
	if len(mapEntries) != 0 {
		setRuntimeStructField(fields, "mapEntries", newSliceValue("Slice<reflect.valueMapEntry>", mapEntries))
	}
	return out, nil
}

func reflectScalarValues(value vmValue) (bool, int64, uint64, float64, complex128, string) {
	switch data := value.materializedData().(type) {
	case bool:
		return data, 0, 0, 0, 0, ""
	case int64:
		return false, data, 0, 0, 0, ""
	case uint64:
		return false, 0, data, 0, 0, ""
	case float64:
		return false, 0, 0, data, 0, ""
	case complex128:
		return false, 0, 0, 0, data, ""
	case string:
		return false, 0, 0, 0, 0, data
	default:
		return false, 0, 0, 0, 0, ""
	}
}

func reflectTypeInfoForValue(module *moduleInstance, value vmValue) TypeInfo {
	if ref, ok := value.payload().(functionRef); ok {
		if target, fn, ok := reflectFunctionRefTarget(module, ref); ok {
			signature := fn.Decl.Signature
			info := reflectTypeInfoForTypeText(target, types.FormatSignature(&target.executable.Artifact.TypeTable, signature))
			info.Kind = "function"
			info.Variadic = fn.Decl.Signature.Variadic
			return info
		}
	}
	if target, ok := value.payload().(reflectMethodTarget); ok {
		signature := target.Method.SignatureType.String()
		info := reflectTypeInfoForTypeText(module, signature)
		info.Kind = "function"
		info.Variadic = target.Method.Variadic
		return info
	}
	if target, ok := value.payload().(reflectUnboundMethodTarget); ok {
		signature := reflectUnboundMethodSignature(
			target.Method.ReceiverType.String(),
			target.Method.SignatureType.String(),
		)
		info := reflectTypeInfoForTypeText(module, signature)
		info.Kind = "function"
		info.QualifiedType = signature
		info.Variadic = target.Method.Variadic
		return info
	}
	typ := strings.TrimSpace(value.Type.String())
	if module != nil {
		if target, name, ok := module.qualifiedTypeModule(typ); ok {
			if decl, ok := target.executable.Types[name]; ok {
				return target.typeInfo(decl)
			}
		}
		if module.executable != nil {
			if decl, ok := module.executable.Types[typ]; ok {
				return module.typeInfo(decl)
			}
		}
	}
	info := reflectTypeInfoForTypeText(module, typ)
	if len(info.Fields) == 0 {
		info.Fields = reflectStructFieldsForValueType(module, typ)
	}
	if len(info.Methods) == 0 {
		info.Methods = reflectPointerMethodsForValueType(module, typ)
	}
	return info
}

func reflectFunctionRefTarget(module *moduleInstance, ref functionRef) (*moduleInstance, loadedFunction, bool) {
	if module == nil {
		return nil, loadedFunction{}, false
	}
	target := module
	modulePath := strings.TrimSpace(ref.ModulePath)
	if modulePath != "" && modulePath != module.modulePath() {
		if module.registry == nil {
			return nil, loadedFunction{}, false
		}
		resolved, ok := module.registry.module(modulePath)
		if !ok {
			return nil, loadedFunction{}, false
		}
		target = resolved
	}
	if target == nil || target.executable == nil {
		return nil, loadedFunction{}, false
	}
	fn, ok := target.executable.Functions[ref.FunctionID]
	return target, fn, ok
}

func reflectPointerMethodsForValueType(module *moduleInstance, typ string) []TypeMethodInfo {
	if module == nil {
		return nil
	}
	elem := reflectPointerElementType(typ)
	if elem == "" {
		return nil
	}
	if target, name, ok := module.qualifiedTypeModule(elem); ok {
		if info, ok := target.findType(name); ok {
			return info.Methods
		}
		return nil
	}
	if info, ok := module.findType(elem); ok {
		return info.Methods
	}
	return nil
}

func reflectPointerElementType(typ string) string {
	elem, ok := coerceRuntimeType(typ).PointerElem()
	if !ok {
		return ""
	}
	return strings.TrimSpace(elem.String())
}

func reflectStructFieldsForValueType(module *moduleInstance, typ string) []TypeFieldInfo {
	fields, ok := module.moduleStructFieldInfo(typ)
	if !ok {
		return nil
	}
	return fields
}
