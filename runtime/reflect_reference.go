package runtime

import "fmt"

func reflectSameReference(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.same_reference expects 2 arguments, got %d", len(args))
	}
	var values [2]vmValue
	for i := range values {
		fields, err := reflectValuePayload(args[i])
		if err != nil {
			return nil, err
		}
		value, ok, err := reflectCurrentValue(fields)
		if err != nil {
			return nil, err
		}
		if !ok {
			return []vmValue{newVMValue("Bool", false)}, nil
		}
		values[i] = value
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.sameRuntimeType(values[0].Type, values[1].Type) {
		return []vmValue{newVMValue("Bool", false)}, nil
	}
	equal := false
	switch left := values[0].Data.(type) {
	case *vmPointer:
		right, ok := values[1].Data.(*vmPointer)
		equal = ok && (left == right || left != nil && right != nil && left.Identity != "" && left.Identity == right.Identity)
	case *vmMap:
		right, ok := values[1].Data.(*vmMap)
		equal = ok && left == right
	case *vmSlice:
		right, ok := values[1].Data.(*vmSlice)
		equal = ok && (left == right || left != nil && right != nil && left.vmSliceStorage != nil && left.vmSliceStorage == right.vmSliceStorage && left.Start == right.Start && left.Len == right.Len)
	}
	return []vmValue{newVMValue("Bool", equal)}, nil
}
