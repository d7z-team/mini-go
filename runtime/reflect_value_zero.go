package runtime

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func reflectValueIsZero(module *moduleInstance, value vmValue) bool {
	data := value.materializedData()
	if data == nil {
		return true
	}
	switch data := data.(type) {
	case bool:
		return !data
	case string:
		return data == ""
	case int64:
		return data == 0
	case uint64:
		return data == 0
	case float64:
		return data == 0
	case complex128:
		return data == 0
	case vmValue:
		// A boxed interface is nonzero even when its dynamic value is zero.
		return false
	case *vmSlice:
		return data == nil
	case *vmArray:
		values := data.values()
		if values == nil {
			return true
		}
		for _, item := range values {
			if !reflectValueIsZero(module, item) {
				return false
			}
		}
		if _, _, ok := module.arrayType(value.Type); ok {
			return true
		}
		return len(values) == 0
	case *vmStruct:
		if data == nil || data.schema == nil {
			return false
		}
		for index, field := range data.schema.fields {
			value := module.zeroValue(field.RuntimeType)
			if stored, ok := data.fieldAt(index); ok {
				value = stored
			}
			if !reflectValueIsZero(module, value) {
				return false
			}
		}
		return true
	case *vmMap:
		return data == nil
	default:
		return false
	}
}

func reflectValueIsNil(value vmValue, info TypeInfo) bool {
	if value.Type.Ref.Kind == types.Any {
		if value.Data == nil {
			return true
		}
		inner, ok := value.Data.(vmValue)
		return ok && reflectValueIsNil(inner, reflectTypeInfoForTypeText(nil, inner.Type.String()))
	}
	switch strings.TrimSpace(info.Kind) {
	case "slice":
		data, ok := value.Data.(*vmSlice)
		return !ok || data == nil
	case "map":
		switch data := value.Data.(type) {
		case nil:
			return true
		case *vmMap:
			return data == nil
		default:
			return false
		}
	case "pointer":
		return value.Data == nil
	case "chan", "recv_chan", "send_chan":
		data, ok := value.Data.(*waitableResource)
		return !ok || data == nil
	case "function":
		return value.Data == nil
	case "interface":
		return value.Data == nil
	default:
		return false
	}
}

func reflectStructFieldValueAt(ctx intrinsicContext, field TypeFieldInfo, index int) vmValue {
	info := reflectTypeInfoForTypeText(reflectRelationModule(ctx), field.RuntimeType.String())
	info.Variadic = field.Variadic
	return newRuntimeStructValue(reflectRelationModule(ctx), "reflect.StructField", map[string]vmValue{
		"Name":      newVMValue("String", field.Name),
		"PkgPath":   newVMValue("String", field.PkgPath),
		"Type":      reflectTypeValueFromVM(ctx.vm, info),
		"Tag":       newVMValue("reflect.StructTag", field.Tag),
		"Offset":    newVMValue("Uintptr", field.Offset),
		"Index":     newSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(index))}),
		"Anonymous": newVMValue("Bool", field.Embedded),
	})
}

func typeFieldRuntimeType(field TypeFieldInfo) string {
	return field.RuntimeType.String()
}

func reflectMethodValueAt(ctx intrinsicContext, owner TypeInfo, method TypeMethodInfo, index int) (vmValue, error) {
	signature := method.SignatureType.String()
	methodFunc := zeroReflectValueValue()
	relationModule := reflectRelationModule(ctx)
	if owner.Kind != "interface" {
		receiver := method.ReceiverType.String()
		signature = reflectUnboundMethodSignature(receiver, signature)
		module, err := reflectMethodModule(ctx, relationModule, method)
		if err != nil {
			return vmValue{}, err
		}
		function := newVMValue(signature, reflectUnboundMethodTarget{Method: method, Module: module})
		methodFunc, err = reflectValueSnapshot(ctx, function)
		if err != nil {
			return vmValue{}, err
		}
	}
	methodTypeInfo := TypeInfo{
		Key:           signature,
		Kind:          "function",
		Type:          signature,
		QualifiedType: signature,
		Display:       reflectSourceType(relationModule, relationModule.resolvedRuntimeType(signature)),
		Variadic:      method.Variadic,
	}
	methodType := reflectTypeValueFromVM(ctx.vm, methodTypeInfo)
	receiverType := reflectTypeValueFromVM(ctx.vm, reflectTypeInfoForTypeText(relationModule, method.ReceiverType.String()))
	signatureType := reflectTypeValueFromVM(ctx.vm, reflectTypeInfoForTypeText(relationModule, method.SignatureType.String()))
	return newRuntimeStructValue(relationModule, "reflect.Method", map[string]vmValue{
		"Name":          newVMValue("String", method.Name),
		"PkgPath":       newVMValue("String", method.PkgPath),
		"Type":          methodType,
		"Func":          methodFunc,
		"Index":         newVMValue("Int", int64(index)),
		"modulePath":    newVMValue("String", method.ModulePath),
		"receiverType":  receiverType,
		"signatureType": signatureType,
		"functionID":    newVMValue("String", method.FunctionID),
		"variadic":      newVMValue("Bool", method.Variadic),
		"exported":      newVMValue("Bool", method.Exported),
	}), nil
}

func reflectUnboundMethodSignature(receiver, signature string) string {
	receiver = strings.TrimSpace(receiver)
	signature = strings.TrimSpace(signature)
	if receiver == "" || !strings.HasPrefix(signature, "function(") {
		return signature
	}
	rest := signature[len("function("):]
	if strings.HasPrefix(rest, ")") {
		return "function(" + receiver + rest
	}
	return "function(" + receiver + ", " + rest
}
