package runtime

import (
	"fmt"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestValueInspectionPreservesPointerAddressPath(t *testing.T) {
	revision := &instanceRevision{generation: 1}
	module := &moduleInstance{revision: revision}
	cell := &slot{module: module, initialized: true, value: newSliceValue("Slice<Any>", []vmValue{
		newVMValue("Function", functionRef{exact: module}), newVMValue("Int", int64(7)),
	})}
	for _, inspect := range []bool{false, true} {
		for _, index := range []int64{0, 1} {
			t.Run(fmt.Sprintf("inspect=%t/index=%d", inspect, index), func(t *testing.T) {
				pointer := newPathPointerValue("Any", "element", cell, []ir.AddressPathSegment{{Kind: "index"}}, []vmValue{newVMValue("Int", index)})
				found := false
				walker := newRuntimeValueWalker(func(actual *instanceRevision) { found = found || actual == revision })
				if inspect {
					walker.enter = func(vmValue) bool { return true }
					walker.leave = func() {}
				}
				walker.value(pointer)
				if found != (index == 0) {
					t.Fatalf("referenced revision=%t for index %d", found, index)
				}
			})
		}
	}
}

func TestRuntimeValueWalkerFindsNestedMethodRevision(t *testing.T) {
	revision := &instanceRevision{generation: 1}
	resourceRevision := &instanceRevision{generation: 2}
	module := &moduleInstance{revision: revision}
	resourceModule := &moduleInstance{revision: resourceRevision}
	target := newVMValue("function() Int", reflectMethodTarget{Module: module, Receiver: newVMValue("Int", int64(1))})
	dynamic := newVMValue("function() Int", reflectMakeFuncTarget{
		functionType: coerceRuntimeType("function() Int"), handlerModule: module,
		handler: functionRef{exact: module, captureSlots: []*slot{
			{value: newVMValue("function() Int", functionRef{exact: resourceModule}), initialized: true},
		}},
	})
	slice := newSliceValue("Slice<Any>", []vmValue{target, dynamic})
	pointer := newSlotPointerValue("Slice<Any>", "slice", &slot{value: slice, initialized: true})
	pointer = newVMValue(pointer.Type, &vmPointer{Type: coerceRuntimeType("Slice<Any>"), original: pointer.Data.(*vmPointer)})
	seen := make(map[*instanceRevision]bool)
	newRuntimeValueWalker(func(found *instanceRevision) { seen[found] = true }).value(pointer)
	if !seen[revision] || !seen[resourceRevision] {
		t.Fatalf("nested values retained revisions = %#v", seen)
	}
}

func TestReflectMethodCallUsesCapturedModule(t *testing.T) {
	oldModule := &moduleInstance{executable: &executable{
		Artifact: ir.Artifact{Module: ir.Module{Path: "example/method"}},
		Functions: map[string]loadedFunction{
			"fn.method.old": {ResultTypes: []vmType{coerceRuntimeType("Int")}},
		},
	}}
	currentModule := &moduleInstance{executable: &executable{
		Artifact:  ir.Artifact{Module: ir.Module{Path: "example/method"}},
		Functions: map[string]loadedFunction{},
	}}
	target := reflectMethodTarget{
		Receiver: newVMValue("Int", int64(1)),
		Method: TypeMethodInfo{
			ModulePath: "example/method", Name: "Value",
			ReceiverType: coerceRuntimeType("Int"), SignatureType: coerceRuntimeType("function() Int"),
			FunctionID: "fn.method.old",
		},
		Module: oldModule,
	}
	request, err := reflectCallMethodTarget(intrinsicContext{vm: &vm{}, module: currentModule}, target, nil, false)
	if err != nil {
		t.Fatalf("method call error = %v", err)
	}
	if request.module != oldModule || request.functionID != "fn.method.old" {
		t.Fatalf("method target = %p %q, want captured module %p", request.module, request.functionID, oldModule)
	}
}
