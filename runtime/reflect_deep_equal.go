package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

type reflectDeepVisit struct {
	pointers map[[2]*vmPointer]bool
	slices   map[[2]*vmSlice]bool
	maps     map[[2]*vmMap]bool
	ctx      intrinsicContext
	count    int
}

func reflectDeepEqual(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.deep_equal expects 2 arguments, got %d", len(args))
	}
	module := reflectRelationModule(ctx)
	left, leftOK, err := reflectDeepDynamic(module, args[0])
	if err != nil {
		return nil, err
	}
	right, rightOK, err := reflectDeepDynamic(module, args[1])
	if err != nil {
		return nil, err
	}
	if !leftOK || !rightOK {
		return []vmValue{newVMValue("Bool", leftOK == rightOK)}, nil
	}
	equal, err := reflectDeepValues(module, left, right, &reflectDeepVisit{
		pointers: make(map[[2]*vmPointer]bool),
		slices:   make(map[[2]*vmSlice]bool),
		maps:     make(map[[2]*vmMap]bool),
		ctx:      ctx,
	})
	if err != nil {
		return nil, err
	}
	return []vmValue{newVMValue("Bool", equal)}, nil
}

func reflectDeepDynamic(module *moduleInstance, value vmValue) (vmValue, bool, error) {
	for i := 0; i < 64; i++ {
		if value.Type.Ref.Kind != types.Any && (module == nil || !module.isInterfaceType(value.Type)) {
			return value, true, nil
		}
		if value.Data == nil {
			return vmValue{}, false, nil
		}
		inner, ok := value.Data.(vmValue)
		if !ok {
			return vmValue{}, false, errors.New("reflect: invalid interface value")
		}
		value = inner
	}
	return vmValue{}, false, errors.New("reflect: interface nesting limit exceeded")
}

func reflectDeepValues(module *moduleInstance, left, right vmValue, visited *reflectDeepVisit) (bool, error) {
	visited.count++
	if visited.count&255 == 1 {
		if err := visited.ctx.cancellationError(); err != nil {
			return false, err
		}
	}
	var err error
	left, leftOK, err := reflectDeepDynamic(module, left)
	if err != nil {
		return false, err
	}
	right, rightOK, err := reflectDeepDynamic(module, right)
	if err != nil || !leftOK || !rightOK {
		return leftOK == rightOK, err
	}
	if module != nil && !module.sameRuntimeType(left.Type, right.Type) || module == nil && left.Type.String() != right.Type.String() {
		return false, nil
	}

	switch leftData := left.materializedData().(type) {
	case bool:
		rightData, ok := right.materializedData().(bool)
		return ok && leftData == rightData, nil
	case string:
		rightData, ok := right.materializedData().(string)
		return ok && leftData == rightData, nil
	case int64:
		rightData, ok := right.materializedData().(int64)
		return ok && leftData == rightData, nil
	case uint64:
		rightData, ok := right.materializedData().(uint64)
		return ok && leftData == rightData, nil
	case float64:
		rightData, ok := right.materializedData().(float64)
		return ok && leftData == rightData, nil
	case complex128:
		rightData, ok := right.Data.(complex128)
		return ok && leftData == rightData, nil
	case *vmPointer:
		rightData, ok := right.Data.(*vmPointer)
		if !ok || leftData == nil || rightData == nil {
			return ok && leftData == nil && rightData == nil, nil
		}
		if leftData == rightData || leftData.Identity != "" && leftData.Identity == rightData.Identity {
			return true, nil
		}
		pair := [2]*vmPointer{leftData, rightData}
		if visited.pointers[pair] {
			return true, nil
		}
		visited.pointers[pair] = true
		leftElem, err := derefPointer(left)
		if err != nil {
			return false, err
		}
		rightElem, err := derefPointer(right)
		if err != nil {
			return false, err
		}
		return reflectDeepValues(module, leftElem, rightElem, visited)
	case *vmSlice:
		rightData, ok := right.Data.(*vmSlice)
		if !ok || leftData == nil || rightData == nil {
			return ok && leftData == nil && rightData == nil, nil
		}
		if leftData.Len != rightData.Len {
			return false, nil
		}
		if reflectSameSliceStart(leftData, rightData) {
			return true, nil
		}
		pair := [2]*vmSlice{leftData, rightData}
		if visited.slices[pair] {
			return true, nil
		}
		visited.slices[pair] = true
		for i := 0; i < leftData.Len; i++ {
			equal, err := reflectDeepValues(module, leftData.valueAt(i), rightData.valueAt(i), visited)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	case *vmMap:
		rightData, ok := right.Data.(*vmMap)
		if !ok || leftData == nil || rightData == nil {
			return ok && leftData == nil && rightData == nil, nil
		}
		if leftData == rightData {
			return true, nil
		}
		pair := [2]*vmMap{leftData, rightData}
		if visited.maps[pair] {
			return true, nil
		}
		visited.maps[pair] = true
		leftEntries, rightEntries := leftData.snapshot(), rightData.snapshot()
		if len(leftEntries) != len(rightEntries) {
			return false, nil
		}
		for key, leftEntry := range leftEntries {
			rightEntry, ok := rightEntries[key]
			if !ok {
				return false, nil
			}
			equal, err := reflectDeepValues(module, leftEntry.Value, rightEntry.Value, visited)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	case *vmArray:
		rightArray, ok := right.Data.(*vmArray)
		if !ok || leftData.Len != rightArray.Len {
			return false, nil
		}
		leftItems, rightItems := leftData.values(), rightArray.values()
		for i := range leftItems {
			equal, err := reflectDeepValues(module, leftItems[i], rightItems[i], visited)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	case *vmStruct:
		rightData, ok := right.Data.(*vmStruct)
		if !ok || leftData == nil || rightData == nil || leftData.schema == nil || rightData.schema == nil ||
			len(leftData.schema.fields) != len(rightData.schema.fields) {
			return false, nil
		}
		for _, field := range leftData.schema.fields {
			leftField, err := loadFieldValue(module, left, field.Name)
			if err != nil {
				return false, nil
			}
			rightField, err := loadFieldValue(module, right, field.Name)
			if err != nil {
				return false, nil
			}
			equal, err := reflectDeepValues(module, leftField, rightField, visited)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	case *mutexResource:
		rightData, ok := right.Data.(*mutexResource)
		return ok && leftData == rightData, nil
	case *waitableResource:
		rightData, ok := right.Data.(*waitableResource)
		return ok && leftData == rightData, nil
	case functionRef, reflectMethodTarget, reflectUnboundMethodTarget, reflectMakeFuncTarget:
		return left.Data == nil && right.Data == nil, nil
	case nil:
		return right.Data == nil, nil
	default:
		rightData := right.materializedData()
		return fmt.Sprintf("%T:%v", leftData, leftData) == fmt.Sprintf("%T:%v", rightData, rightData), nil
	}
}

func reflectSameSliceStart(left, right *vmSlice) bool {
	if left == right {
		return true
	}
	if left == nil || right == nil || left.ByteBacked != right.ByteBacked || left.Start != right.Start {
		return false
	}
	if left.ByteBacked {
		return left.Start < len(left.ByteBacking) && right.Start < len(right.ByteBacking) &&
			&left.ByteBacking[left.Start] == &right.ByteBacking[right.Start]
	}
	return left.Start < len(left.Backing) && right.Start < len(right.Backing) &&
		&left.Backing[left.Start] == &right.Backing[right.Start]
}
