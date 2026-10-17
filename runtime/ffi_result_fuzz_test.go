package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/d7z-team/mini-go/ffi"
)

func FuzzFFICallResultDisposition(f *testing.F) {
	f.Add([]byte{0, 1}, byte(0))
	f.Add([]byte{1, 0}, byte(0))
	f.Add([]byte{0, 2, 1}, byte(0))
	f.Add([]byte{0, 2}, byte(1))
	f.Add([]byte{0, 2}, byte(2))
	f.Add([]byte{0, 2, 1}, byte(3))
	f.Add([]byte{1, 0, 2}, byte(6))
	f.Fuzz(func(t *testing.T, operations []byte, limits byte) {
		if len(operations) > 64 {
			return
		}
		module := newManagedValueTestModule()
		machine := module.vm
		machine.limits = normalizeLimits(Limits{MaxBoundaryBytes: 32, MaxGuestBytes: 1 << 20})
		machine.ffiCalls = make(map[*pendingFFICall]struct{})
		machine.owner.Store(true)
		if limits%3 == 1 {
			machine.limits.MaxBoundaryBytes = 1
		}
		_, cancel := context.WithCancel(context.Background())
		pending := &pendingFFICall{vm: machine, cancel: cancel, state: pendingFFIWaiting}
		relay := &ffiCompletionRelay{pending: pending}
		pending.relay = relay
		machine.ffiCalls[pending] = struct{}{}
		machine.pendingEvents.Store(1)
		completed, consumed, discarded := false, false, 0
		var resultError error
		switch limits / 3 % 3 {
		case 1:
			resultError = ffi.ErrRouteUnavailable
		case 2:
			resultError = errors.New("host failed")
		}
		for _, operation := range operations {
			switch operation % 3 {
			case 0:
				if !completed {
					completed = true
					relay.complete(ffi.Result{Payload: []byte("lease"), Err: resultError, Discard: func() { discarded++ }})
				}
			case 1:
				pending.stop()
			case 2:
				values, ready, admissionErr := pending.materializeForFrame(module, nil)
				if admissionErr != nil {
					t.Fatalf("host reply consumption failed: %v", admissionErr)
				}
				if ready {
					status, err := numericAsInt64(values[2])
					if err != nil {
						t.Fatal(err)
					}
					consumed = status == 0
					if resultError != nil {
						wantStatus := int64(2)
						if errors.Is(resultError, ffi.ErrRouteUnavailable) {
							wantStatus = 1
						}
						if status != wantStatus {
							t.Fatalf("status = %#v, want %d", values[2], wantStatus)
						}
					}
				}
			}
		}
		pending.stop()
		want := 0
		if completed && !consumed {
			want = 1
		}
		if discarded != want {
			t.Fatalf("operations=%v limits=%d: discarded=%d want=%d", operations, limits, discarded, want)
		}
		if len(machine.ffiCalls) != 0 || machine.pendingEvents.Load() != 0 || machine.pendingBoundaryBytes.Load() != 0 {
			t.Fatal("terminal FFI call retained boundary state")
		}
	})
}

func TestFFICallCompletionRacesCancellation(t *testing.T) {
	for range 100 {
		machine := &vm{limits: normalizeLimits(Limits{}), ffiCalls: make(map[*pendingFFICall]struct{})}
		_, cancel := context.WithCancel(context.Background())
		pending := &pendingFFICall{vm: machine, cancel: cancel}
		relay := &ffiCompletionRelay{pending: pending}
		pending.relay = relay
		machine.ffiCalls[pending] = struct{}{}
		machine.pendingEvents.Store(1)
		var discards atomic.Int32
		var operations sync.WaitGroup
		operations.Add(2)
		go func() {
			defer operations.Done()
			relay.complete(ffi.Result{Payload: []byte("lease"), Discard: func() { discards.Add(1) }})
		}()
		go func() {
			defer operations.Done()
			pending.stop()
		}()
		operations.Wait()
		if discards.Load() != 1 || len(machine.ffiCalls) != 0 || machine.pendingEvents.Load() != 0 || machine.pendingBoundaryBytes.Load() != 0 {
			t.Fatal("cancel/complete race failed to release the result exactly once")
		}
	}
}
