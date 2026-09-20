package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/ffi"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func ffiPayloadBytes(module *moduleInstance, value vmValue) ([]byte, error) {
	elem, ok := value.Type.SliceElem()
	if !ok || !elem.Primitive(types.PrimitiveUint8) {
		return nil, fmt.Errorf("FFI payload must be []byte, got %s", value.Type)
	}
	if value.Data == nil {
		return nil, nil
	}
	slice, ok := value.Data.(*vmSlice)
	if !ok {
		return nil, fmt.Errorf("invalid FFI byte slice %T", value.Data)
	}
	if slice == nil {
		return nil, nil
	}
	if slice.ByteBacked {
		return slice.bytes(), nil
	}
	items, ok := sliceValues(value)
	if !ok {
		return nil, fmt.Errorf("invalid FFI byte slice %T", value.Data)
	}
	out := make([]byte, len(items))
	for i, item := range items {
		item, err := module.coerceAssignableValue(item, "Uint8")
		if err != nil {
			return nil, fmt.Errorf("FFI payload byte %d: %w", i, err)
		}
		n, err := numericAsUint64(item)
		if err != nil || n > 255 {
			return nil, fmt.Errorf("FFI payload byte %d is not uint8", i)
		}
		out[i] = byte(n)
	}
	return out, nil
}

type pendingFFICall struct {
	mu            sync.Mutex
	vm            *vm
	call          ffi.Call
	cancel        context.CancelFunc
	result        ffi.Result
	state         pendingFFIState
	starting      bool
	scope         *executionScope
	relay         *ffiCompletionRelay
	boundaryBytes int64
	waitOwner     *taskOwnership
	waitTicket    *taskWaitTicket
}

func (call *pendingFFICall) bindWait(owner *taskOwnership, ticket *taskWaitTicket) {
	call.mu.Lock()
	call.waitOwner, call.waitTicket = owner, ticket
	ready := call.state == pendingFFIReady && !call.starting
	call.mu.Unlock()
	if ready {
		owner.notify(ticket)
	}
}

type pendingFFIState uint8

const (
	pendingFFIWaiting pendingFFIState = iota
	pendingFFIReady
	pendingFFIConsumed
	pendingFFICanceled
)

type ffiCompletionRelay struct {
	mu      sync.Mutex
	pending *pendingFFICall
}

func (relay *ffiCompletionRelay) complete(result ffi.Result) {
	relay.mu.Lock()
	pending := relay.pending
	relay.mu.Unlock()
	if pending != nil {
		pending.complete(result)
	} else if result.Discard != nil {
		result.Discard()
	}
}

func (relay *ffiCompletionRelay) detach() {
	if relay == nil {
		return
	}
	relay.mu.Lock()
	relay.pending = nil
	relay.mu.Unlock()
}

func (call *pendingFFICall) complete(result ffi.Result) {
	call.mu.Lock()
	if call.state != pendingFFIWaiting {
		call.mu.Unlock()
		if result.Discard != nil {
			result.Discard()
		}
		return
	}
	discard := result.Discard
	if result.Err != nil {
		result.Payload = nil
	} else if machine := call.vm; machine != nil {
		if int64(len(result.Payload)) > machine.limits.MaxBoundaryBytes {
			result = ffi.Result{Err: ResourceLimitError{
				Code:    "execution.boundary_limit",
				Message: fmt.Sprintf("FFI response byte limit exceeded: max %d", machine.limits.MaxBoundaryBytes),
			}}
		} else if err := machine.reserveBoundaryBytes(int64(len(result.Payload)) * ir.RuntimeByteBytes); err != nil {
			result = ffi.Result{Err: err}
		} else {
			call.boundaryBytes += int64(len(result.Payload)) * ir.RuntimeByteBytes
		}
	}
	if result.Err != nil {
		result.Discard = nil
	}
	call.result = result
	call.state = pendingFFIReady
	machine := call.vm
	owner, ticket := call.waitOwner, call.waitTicket
	starting := call.starting
	call.mu.Unlock()
	if owner != nil && !starting {
		owner.notify(ticket)
	}
	if result.Err != nil && discard != nil {
		discard()
	}
	if machine != nil && !starting {
		machine.signalWake()
	}
}

// finishStart commits the host handle before publishing an early reply. A
// failed start owns disposal of that reply even if completion arrived first.
func (call *pendingFFICall) finishStart(hostCall ffi.Call, startErr error) {
	call.mu.Lock()
	call.starting = false
	if call.state == pendingFFICanceled || call.state == pendingFFIConsumed {
		call.mu.Unlock()
		if hostCall != nil {
			hostCall.Cancel()
		}
		return
	}
	var discard func()
	if startErr != nil {
		discard = call.result.Discard
		responseBytes := int64(len(call.result.Payload)) * ir.RuntimeByteBytes
		call.boundaryBytes -= responseBytes
		if call.vm != nil {
			call.vm.pendingBoundaryBytes.Add(-responseBytes)
		}
		call.result = ffi.Result{Err: startErr}
		call.state = pendingFFIReady
	} else {
		call.call = hostCall
	}
	ready := call.state == pendingFFIReady
	machine, owner, ticket := call.vm, call.waitOwner, call.waitTicket
	call.mu.Unlock()
	if startErr != nil && hostCall != nil {
		hostCall.Cancel()
	}
	if discard != nil {
		discard()
	}
	if ready {
		if owner != nil {
			owner.notify(ticket)
		}
		if machine != nil {
			machine.signalWake()
		}
	}
}

func (vm *vm) startFFICall(scope *executionScope, route string, payload []byte) (*pendingFFICall, error) {
	if vm == nil || vm.machine == nil {
		return nil, errors.New("FFI call requires an active execution")
	}
	if scope == nil || scope.settled {
		return nil, errors.New("FFI call requires an active execution scope")
	}
	if int64(len(payload)) > vm.limits.MaxBoundaryBytes {
		return nil, ResourceLimitError{
			Code:    "execution.boundary_limit",
			Message: fmt.Sprintf("FFI request byte limit exceeded: max %d", vm.limits.MaxBoundaryBytes),
		}
	}
	if err := vm.reservePendingEvent(); err != nil {
		return nil, err
	}
	requestBytes := int64(len(payload)) * ir.RuntimeByteBytes
	if err := vm.reserveBoundaryBytes(requestBytes); err != nil {
		vm.pendingEvents.Add(-1)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending := &pendingFFICall{
		vm: vm, cancel: cancel, scope: scope,
		boundaryBytes: requestBytes, starting: true,
	}
	relay := &ffiCompletionRelay{pending: pending}
	pending.relay = relay
	if vm.ffiCalls == nil {
		vm.ffiCalls = make(map[*pendingFFICall]struct{})
	}
	vm.ffiCalls[pending] = struct{}{}
	vm.machine.addScopeFFI(scope)
	if vm.ffiSession == nil {
		pending.finishStart(nil, ffi.ErrRouteUnavailable)
		return pending, nil
	}
	call, err := vm.ffiSession.Start(ctx, ffi.Request{Route: route, Payload: payload}, relay.complete)
	pending.finishStart(call, err)
	return pending, nil
}

func (call *pendingFFICall) take() ([]vmValue, bool) {
	if call == nil {
		return []vmValue{newByteSliceValue("Slice<Uint8>", ""), newVMValue("String", "FFI call lost continuation"), newVMValue("Int", int64(2))}, true
	}
	call.mu.Lock()
	ready := call.state == pendingFFIReady && !call.starting
	machine := call.vm
	payloadBytes := int64(len(call.result.Payload)) * ir.RuntimeByteBytes
	call.mu.Unlock()
	var allocationErr error
	if ready && machine != nil && payloadBytes > 0 {
		allocationErr = machine.chargeAllocationBytes(ir.RuntimeNodeBytes + payloadBytes)
	}
	result, settled := call.settle(pendingFFIConsumed)
	if !settled {
		return nil, false
	}
	if allocationErr != nil {
		if result.Discard != nil {
			result.Discard()
		}
		result = ffi.Result{Err: allocationErr}
	}
	message := ""
	status := int64(0)
	if result.Err != nil {
		message = result.Err.Error()
		status = 2
		if errors.Is(result.Err, ffi.ErrRouteUnavailable) {
			status = 1
		}
	}
	return []vmValue{
		newByteSliceHeaderValue("Slice<Uint8>", result.Payload, len(result.Payload), len(result.Payload)),
		newVMValue("String", message),
		newVMValue("Int", status),
	}, true
}

func (call *pendingFFICall) settle(next pendingFFIState) (ffi.Result, bool) {
	call.mu.Lock()
	valid := false
	switch next {
	case pendingFFIConsumed:
		valid = call.state == pendingFFIReady && !call.starting
	case pendingFFICanceled:
		valid = call.state != pendingFFIConsumed && call.state != pendingFFICanceled
	}
	if !valid {
		call.mu.Unlock()
		return ffi.Result{}, false
	}
	call.state = next
	call.cancel()
	result := call.result
	call.result = ffi.Result{}
	machine, scope, relay, hostCall, boundaryBytes := call.vm, call.scope, call.relay, call.call, call.boundaryBytes
	call.vm, call.scope, call.relay, call.call = nil, nil, nil, nil
	call.waitOwner, call.waitTicket = nil, nil
	call.boundaryBytes = 0
	call.mu.Unlock()
	relay.detach()
	if machine != nil {
		machine.releasePendingEvent(boundaryBytes)
		delete(machine.ffiCalls, call)
		if machine.machine != nil {
			machine.machine.releaseScopeFFI(scope)
		}
	}
	if next == pendingFFICanceled && hostCall != nil {
		hostCall.Cancel()
	}
	if next == pendingFFICanceled && result.Discard != nil {
		result.Discard()
	}
	return result, true
}

func (call *pendingFFICall) stop() {
	if call == nil {
		return
	}
	_, _ = call.settle(pendingFFICanceled)
}

func (vm *vm) closeFFICalls() {
	if vm == nil {
		return
	}
	for call := range vm.ffiCalls {
		call.stop()
	}
	clear(vm.ffiCalls)
}

func (vm *vm) cancelScopeFFICalls(scopeID int64) {
	for call := range vm.ffiCalls {
		if call != nil && call.scope != nil && call.scope.id == scopeID {
			call.stop()
		}
	}
}

func (vm *vm) cancelScopeResources(scopeID int64) {
	if vm == nil || scopeID == 0 {
		return
	}
	vm.cancelScopeTimers(scopeID)
	vm.cancelScopeFFICalls(scopeID)
}
