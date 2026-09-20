package runtime

import (
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestReflectValuePayloadAllowsSparseDerivedFields(t *testing.T) {
	value := zeroReflectValueValue()
	if _, err := reflectValuePayload(value); err != nil {
		t.Fatalf("zero Value payload: %v", err)
	}
	fields := value.Data.(*vmStruct)
	capacity, _, _ := fields.schema.field("capacity")
	fields.values[capacity] = vmValue{}
	if _, err := reflectValuePayload(value); err != nil {
		t.Fatalf("sparse Value payload: %v", err)
	}
	data, _, _ := fields.schema.field("data")
	fields.values[data] = vmValue{}
	if _, err := reflectValuePayload(value); err == nil || !strings.Contains(err.Error(), "data") {
		t.Fatalf("missing Value data error = %v", err)
	}
}

func TestReflectTypePayloadRequiresCanonicalKey(t *testing.T) {
	incomplete := newVMValue("reflect.Type", newRuntimeStructValue(nil, "reflect.runtimeType", map[string]vmValue{
		"type": newVMValue("String", "Int"),
	}))
	if _, err := reflectTypeKeyFromValue(incomplete); err == nil || !strings.Contains(err.Error(), "canonical key") {
		t.Fatalf("incomplete Type payload error = %v", err)
	}
}

func TestReflectMethodPayloadUsesStructuredTypes(t *testing.T) {
	machine := &vm{}
	module := &moduleInstance{executable: &executable{Artifact: ir.Artifact{Module: ir.Module{Path: "example/main"}}}}
	ctx := intrinsicContext{vm: machine, module: module}
	method := TypeMethodInfo{
		ModulePath: "example/main", Name: "Call",
		ReceiverType:  coerceRuntimeType("example/main.Service"),
		SignatureType: coerceRuntimeType("function(Int) String"),
		FunctionID:    "method.Service.Call", Exported: true,
	}
	value, err := reflectMethodValueAt(ctx, TypeInfo{Kind: "interface"}, method, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := reflectMethodInfoFromValue(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ReceiverType.String() != method.ReceiverType.String() || decoded.SignatureType.String() != method.SignatureType.String() {
		t.Fatalf("decoded method types = %s, %s", decoded.ReceiverType, decoded.SignatureType)
	}
	if _, ok := updatedStructValue(value.Data, "receiverType", newVMValue("String", method.ReceiverType.String())); !ok {
		t.Fatal("update receiverType")
	}
	if _, err := reflectMethodInfoFromValue(ctx, value); err == nil || !strings.Contains(err.Error(), "receiverType") {
		t.Fatalf("string receiver metadata error = %v", err)
	}
}
