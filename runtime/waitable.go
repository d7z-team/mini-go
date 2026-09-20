package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

// waitableResource is the runtime's concrete implementation of the language-neutral
// waitable resource protocol. Source-level resource/select semantics are
// lowered by the compiler; this object only stores resource state and waiters.
type waitableResource struct {
	selectHead, selectTail *channelSelectCase
	Type                   vmType
	ElemType               vmType
	Capacity               int
	Buffer                 []vmValue
	bufferHead             int
	Closed                 bool
}

func makeWaitableValue(module *moduleInstance, typ any, capacity vmValue) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	elemType := module.resolvedRuntimeType("Any")
	if info, ok := runtimeType.WaitableInfo(); ok {
		elemType = info.Elem
	}
	n, err := asInt64(capacity)
	if err != nil {
		return vmValue{}, err
	}
	if n < 0 {
		return vmValue{}, newGuestPanic(fmt.Errorf("waitable capacity must be non-negative, got %d", n))
	}
	_, capacityInt, err := module.vm.checkCollectionSize(0, n)
	if err != nil {
		return vmValue{}, err
	}
	if module.vm != nil {
		if err := module.vm.chargeRuntimeObject(capacityInt, 0); err != nil {
			return vmValue{}, err
		}
	}
	resource := &waitableResource{
		Type:     runtimeType,
		ElemType: elemType,
		Capacity: capacityInt,
		Buffer:   make([]vmValue, 0, capacityInt),
	}
	return newVMValue(runtimeType, resource), nil
}

func waitableTrySendValue(module *moduleInstance, waitableValue, value vmValue) (bool, error) {
	resource, err := waitableValueData(module, waitableValue)
	if err != nil {
		return false, err
	}
	if !module.waitableCanSend(waitableValue.Type) {
		return false, fmt.Errorf("cannot send on receive-only waitable %s", waitableValue.Type)
	}
	if resource == nil {
		return false, nil
	}
	if resource.Closed {
		return false, newGuestPanic(errors.New("send on closed waitable"))
	}
	normalized, err := normalizeWaitableSendValue(module, resource, value)
	if err != nil {
		return false, err
	}
	if peer := resource.selectionPeer(true, nil); peer != nil {
		peer.selection.complete(peer.index, normalized, true)
		return true, nil
	}
	if resource.bufferLen() >= resource.Capacity {
		return false, nil
	}
	resource.appendBuffer(normalized)
	return true, nil
}

func waitableTryRecvValues(module *moduleInstance, waitableValue vmValue) (vmValue, vmValue, error) {
	value, ok, _, err := waitableTryRecvValue(module, waitableValue)
	if err != nil {
		return vmValue{}, vmValue{}, err
	}
	return value, newVMValue("Bool", ok), nil
}

func waitableReadyRecvValue(module *moduleInstance, waitableValue vmValue) (vmValue, error) {
	resource, err := waitableValueData(module, waitableValue)
	if err != nil {
		return vmValue{}, err
	}
	if !module.waitableCanRecv(waitableValue.Type) {
		return vmValue{}, fmt.Errorf("cannot receive from send-only waitable %s", waitableValue.Type)
	}
	return newVMValue("Bool", waitableRecvReady(resource)), nil
}

func waitableReadySendValue(module *moduleInstance, waitableValue vmValue) (vmValue, error) {
	resource, err := waitableValueData(module, waitableValue)
	if err != nil {
		return vmValue{}, err
	}
	if !module.waitableCanSend(waitableValue.Type) {
		return vmValue{}, fmt.Errorf("cannot send on receive-only waitable %s", waitableValue.Type)
	}
	return newVMValue("Bool", waitableSendReady(resource)), nil
}

func waitableTryRecvValue(module *moduleInstance, waitableValue vmValue) (vmValue, bool, bool, error) {
	resource, err := waitableValueData(module, waitableValue)
	if err != nil {
		return vmValue{}, false, false, err
	}
	if !module.waitableCanRecv(waitableValue.Type) {
		return vmValue{}, false, false, fmt.Errorf("cannot receive from send-only waitable %s", waitableValue.Type)
	}
	if resource == nil {
		return waitableZeroValue(module, waitableValue.Type, nil), false, false, nil
	}
	if resource.bufferLen() == 0 {
		if !resource.Closed {
			if peer := resource.selectionPeer(false, nil); peer != nil {
				value := peer.value
				peer.selection.complete(peer.index, vmValue{}, false)
				return value, true, false, nil
			}
		}
		return waitableZeroValue(module, waitableValue.Type, resource), false, resource.Closed, nil
	}
	value := resource.popBuffer()
	return value, true, resource.Closed, nil
}

func waitableCloseValue(module *moduleInstance, waitableValue vmValue) error {
	resource, err := waitableValueData(module, waitableValue)
	if err != nil {
		return err
	}
	if !module.waitableCanSend(waitableValue.Type) {
		return fmt.Errorf("cannot close receive-only waitable %s", waitableValue.Type)
	}
	if resource == nil {
		return newGuestPanic(errors.New("close of nil waitable"))
	}
	if resource.Closed {
		return newGuestPanic(errors.New("close of closed waitable"))
	}
	resource.Closed = true
	resource.notifySelections()
	return nil
}

func waitableRecvReady(resource *waitableResource) bool {
	if resource == nil {
		return false
	}
	return resource.bufferLen() != 0 || resource.Closed || resource.selectionPeer(false, nil) != nil
}

func waitableSendReady(resource *waitableResource) bool {
	if resource == nil {
		return false
	}
	if resource.Closed {
		return true
	}
	if resource.selectionPeer(true, nil) != nil {
		return true
	}
	return resource.bufferLen() < resource.Capacity
}

func waitableLenValue(resource *waitableResource) vmValue {
	if resource == nil {
		return newVMValue("Int", int64(0))
	}
	return newVMValue("Int", int64(resource.bufferLen()))
}

func waitableCapValue(resource *waitableResource) vmValue {
	if resource == nil {
		return newVMValue("Int", int64(0))
	}
	return newVMValue("Int", int64(resource.Capacity))
}

func (resource *waitableResource) bufferLen() int {
	if resource == nil {
		return 0
	}
	return len(resource.Buffer) - resource.bufferHead
}

func (resource *waitableResource) appendBuffer(value vmValue) {
	if resource.bufferHead > 0 && len(resource.Buffer) == cap(resource.Buffer) {
		active := copy(resource.Buffer, resource.Buffer[resource.bufferHead:])
		clear(resource.Buffer[active:])
		resource.Buffer = resource.Buffer[:active]
		resource.bufferHead = 0
	}
	resource.Buffer = append(resource.Buffer, value)
	resource.notifySelections()
}

func (resource *waitableResource) popBuffer() vmValue {
	value := resource.Buffer[resource.bufferHead]
	resource.Buffer[resource.bufferHead] = vmValue{}
	resource.bufferHead++
	if resource.bufferHead == len(resource.Buffer) {
		resource.Buffer = resource.Buffer[:0]
		resource.bufferHead = 0
	}
	resource.notifySelections()
	return value
}

func waitableValueData(module *moduleInstance, value vmValue) (*waitableResource, error) {
	resource, ok := value.Data.(*waitableResource)
	if ok {
		return resource, nil
	}
	if value.Data != nil {
		return nil, fmt.Errorf("invalid waitable resource backing for %s", value.Type)
	}
	if !module.isWaitableTypeName(value.Type) {
		return nil, fmt.Errorf("expected waitable resource, got %s", value.Type)
	}
	return nil, nil
}

func normalizeWaitableSendValue(module *moduleInstance, resource *waitableResource, value vmValue) (vmValue, error) {
	elemType := resource.ElemType
	if !elemType.Valid() {
		elemType = module.resolvedRuntimeType("Any")
	}
	normalized, err := module.coerceAssignableValue(value, elemType)
	if err != nil {
		return vmValue{}, fmt.Errorf("waitable send value: %w", err)
	}
	return module.cloneValueForStore(normalized), nil
}

func waitableZeroValue(module *moduleInstance, waitableType any, resource *waitableResource) vmValue {
	var elemType any = "Any"
	if resource != nil && resource.ElemType.Valid() {
		elemType = resource.ElemType
	} else if module != nil {
		if info, ok := module.resolvedRuntimeType(waitableType).WaitableInfo(); ok {
			elemType = info.Elem
		}
	} else if info, ok := coerceRuntimeType(waitableType).WaitableInfo(); ok {
		elemType = info.Elem
	}
	if module != nil {
		return module.zeroValue(elemType)
	}
	return zeroVMValue(coerceRuntimeType(elemType).String())
}

func (m *moduleInstance) waitableCanSend(typ any) bool {
	info, ok := m.resolvedRuntimeType(typ).WaitableInfo()
	return !ok || info.Direction != types.ChannelReceive
}

func (m *moduleInstance) waitableCanRecv(typ any) bool {
	info, ok := m.resolvedRuntimeType(typ).WaitableInfo()
	return !ok || info.Direction != types.ChannelSend
}

func (m *moduleInstance) isWaitableTypeName(typ any) bool {
	_, ok := m.resolvedRuntimeType(typ).WaitableInfo()
	return ok
}

func (resource *waitableResource) notifySelections() {
	for selected := resource.selectHead; selected != nil; selected = selected.next {
		if selection := selected.selection; selection.owner != nil {
			selection.owner.notify(selection.ticket)
		}
	}
}
