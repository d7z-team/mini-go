package runtime

import (
	"errors"

	"github.com/d7z-team/mini-go/compiler/types"
)

// These operations query runtime types directly. They do not publish guest
// reflect.Type/Value objects or require the reflect source package.
func reflectInspectDescribe(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	module := reflectRelationModule(ctx)
	if module == nil {
		return nil, errors.New("reflect/inspect: missing module context")
	}
	value, present, err := reflectDynamicValue(ctx, args[0])
	if err != nil {
		return nil, err
	}
	typ := value.Type
	isNil := !present || isNilRuntimeValue(value)
	if args[1].Data == true {
		typ, present = module.resolvedRuntimeType(typ).Underlying().PointerElem()
	}
	var kind uint64
	var comparable bool
	if present {
		typ = module.resolvedRuntimeType(typ)
		kind = uint64(reflectKindCode(typ.Underlying()))
		comparable, err = module.isComparableRuntimeType(typ, map[string]struct{}{})
		if err != nil {
			return nil, err
		}
	}
	return []vmValue{newVMValue("Uint", kind), newVMValue("Bool", comparable), newVMValue("Bool", isNil)}, nil
}

func reflectInspectElementImplements(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	module := reflectRelationModule(ctx)
	if module == nil {
		return nil, errors.New("reflect/inspect: missing module context")
	}
	var elements [2]vmType
	for i, arg := range args {
		value, present, err := reflectDynamicValue(ctx, arg)
		if err != nil {
			return nil, err
		}
		if !present {
			return []vmValue{newVMValue("Bool", false)}, nil
		}
		element, ok := module.resolvedRuntimeType(value.Type).Underlying().PointerElem()
		if !ok {
			return []vmValue{newVMValue("Bool", false)}, nil
		}
		elements[i] = element
	}
	answer := module.isInterfaceType(elements[1]) && module.implementsInterface(elements[0], elements[1].String())
	return []vmValue{newVMValue("Bool", answer)}, nil
}

func reflectInspectAssign(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	module := reflectRelationModule(ctx)
	if module == nil {
		return nil, errors.New("reflect/inspect: missing module context")
	}
	target, present, err := reflectDynamicValue(ctx, args[0])
	if err != nil {
		return nil, err
	}
	_, pointerType := module.resolvedRuntimeType(target.Type).Underlying().PointerElem()
	if !present || isNilRuntimeValue(target) || !pointerType {
		return []vmValue{newVMValue("Bool", false)}, nil
	}
	pointer, err := pointerValue(target)
	if err != nil {
		return nil, err
	}
	source, present, err := reflectDynamicValue(ctx, args[1])
	if err != nil {
		return nil, err
	}
	if !present {
		if !module.nilAssignableRuntimeType(pointer.Type) {
			return []vmValue{newVMValue("Bool", false)}, nil
		}
		source = module.zeroValue(pointer.Type)
	} else {
		// Signature coercion also handles untyped function references. It must
		// not erase the identity of two distinct defined function types.
		if source.Type.Ref.Kind == types.Named && pointer.Type.Ref.Kind == types.Named &&
			!module.isInterfaceType(pointer.Type) && !module.sameRuntimeType(source.Type, pointer.Type) {
			return []vmValue{newVMValue("Bool", false)}, nil
		}
		source, err = module.coerceAssignableValue(source, pointer.Type)
		if err != nil {
			return []vmValue{newVMValue("Bool", false)}, nil
		}
	}
	if err := pointer.storeValue(source, false); err != nil {
		return nil, err
	}
	return []vmValue{newVMValue("Bool", true)}, nil
}
