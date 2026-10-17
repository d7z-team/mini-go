package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

type reflectDeepVisit struct {
	storage map[[2]reflectStorageIdentity]bool
	ctx     intrinsicContext
	count   int
}

type reflectStorageIdentity struct {
	heap   *managedHeap
	ref    guestRef
	offset uint32
	length int
	host   any
	kind   uint8
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
		storage: make(map[[2]reflectStorageIdentity]bool),
		ctx:     ctx,
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
		if value.payload() == nil {
			return vmValue{}, false, nil
		}
		inner, ok := value.payload().(vmValue)
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
		rightData, ok := right.payload().(complex128)
		return ok && leftData == rightData, nil
	case *vmPointer:
		rightData, ok := right.payload().(*vmPointer)
		if !ok || leftData == nil || rightData == nil {
			return ok && leftData == nil && rightData == nil, nil
		}
		if leftData == rightData || leftData.Identity != "" && leftData.Identity == rightData.Identity {
			return true, nil
		}
		pair := [2]reflectStorageIdentity{}
		for index, pointer := range []*vmPointer{leftData, rightData} {
			pair[index].kind = 1
			if pointer.owner != nil && pointer.storage.valid() {
				pair[index].heap = pointer.owner.heap
				pair[index].ref = pointer.storage
				pair[index].offset = pointer.offset
			} else if pointer.Identity != "" {
				pair[index].host = pointer.Identity
			} else {
				pair[index].host = pointer
			}
		}
		if pair[0] == pair[1] || visited.storage[pair] {
			return true, nil
		}
		visited.storage[pair] = true
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
		rightData, ok := right.payload().(*vmSlice)
		if !ok || leftData == nil || rightData == nil {
			return ok && leftData == nil && rightData == nil, nil
		}
		if leftData.Len != rightData.Len {
			return false, nil
		}
		if reflectSameSliceStart(leftData, rightData) {
			return true, nil
		}
		pair := [2]reflectStorageIdentity{}
		for index, slice := range []*vmSlice{leftData, rightData} {
			pair[index] = reflectStorageIdentity{
				heap: slice.managedHeap, ref: slice.managedRef,
				offset: slice.managedBase + uint32(slice.Start)*slice.managedElem.Stride, length: slice.Len,
			}
			if slice.managedHeap == nil {
				pair[index].host = slice.vmSliceStorage
			}
		}
		if pair[0] == pair[1] || visited.storage[pair] {
			return true, nil
		}
		visited.storage[pair] = true
		for i := 0; i < leftData.Len; i++ {
			equal, err := reflectDeepValues(module, leftData.valueAt(i), rightData.valueAt(i), visited)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	case *vmMap:
		rightData, ok := right.payload().(*vmMap)
		if !ok || leftData == nil || rightData == nil {
			return ok && leftData == nil && rightData == nil, nil
		}
		if leftData == rightData {
			return true, nil
		}
		pair := [2]reflectStorageIdentity{}
		for index, data := range []*vmMap{leftData, rightData} {
			if data.managed != nil {
				pair[index] = reflectStorageIdentity{heap: data.managed.heap, ref: data.managed.identity}
			} else {
				pair[index].host = data
			}
		}
		if pair[0] == pair[1] || visited.storage[pair] {
			return true, nil
		}
		visited.storage[pair] = true
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
		rightArray, ok := right.payload().(*vmArray)
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
		rightData, ok := right.payload().(*vmStruct)
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
		rightData, ok := right.payload().(*mutexResource)
		return ok && leftData == rightData, nil
	case *waitableResource:
		rightData, ok := right.payload().(*waitableResource)
		return ok && leftData == rightData, nil
	case functionRef, reflectMethodTarget, reflectUnboundMethodTarget, reflectMakeFuncTarget:
		return left.payload() == nil && right.payload() == nil, nil
	case nil:
		return right.payload() == nil, nil
	default:
		rightData := right.materializedData()
		return fmt.Sprintf("%T:%v", leftData, leftData) == fmt.Sprintf("%T:%v", rightData, rightData), nil
	}
}

func reflectSameSliceStart(left, right *vmSlice) bool {
	if left == right {
		return true
	}
	if left != nil && right != nil && left.managedHeap != nil && right.managedHeap != nil {
		return left.managedHeap == right.managedHeap && left.managedRef == right.managedRef &&
			left.managedBase+uint32(left.Start)*left.managedElem.Stride == right.managedBase+uint32(right.Start)*right.managedElem.Stride
	}
	if left == nil || right == nil || left.byteBacked() != right.byteBacked() || left.Start != right.Start {
		return false
	}
	if left.byteBacked() {
		return left.vmSliceStorage != nil && right.vmSliceStorage != nil &&
			left.Start < left.vmSliceStorage.capacity && right.Start < right.vmSliceStorage.capacity &&
			left.vmSliceStorage == right.vmSliceStorage
	}
	return left.vmSliceStorage != nil && right.vmSliceStorage != nil &&
		left.Start < left.vmSliceStorage.capacity && right.Start < right.vmSliceStorage.capacity &&
		left.vmSliceStorage == right.vmSliceStorage
}
