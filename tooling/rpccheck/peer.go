// Package rpccheck owns the independent RPC conformance fixtures and peer behavior.
package rpccheck

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/d7z-team/mini-go/rpc"
	service "github.com/d7z-team/mini-go/testdata/rpc/generated/go/service"
	types "github.com/d7z-team/mini-go/testdata/rpc/generated/go/types"
)

type (
	Laboratory struct{ Released atomic.Int64 }
	counter    struct {
		value     atomic.Int64
		owner     *Laboratory
		failClose atomic.Bool
	}
)

func (c *counter) Add(_ context.Context, delta int64) (int64, error) { return c.value.Add(delta), nil }

func (c *counter) Close(context.Context) error {
	if c.failClose.Swap(false) {
		return rpc.StatusError{Code: rpc.CodeFailedPrecondition, Message: "injected close failure"}
	}
	c.owner.Released.Add(1)
	return nil
}

func (l *Laboratory) Echo(_ context.Context, value service.Packet) (service.Packet, error) {
	return value, nil
}

func (l *Laboratory) Tree(_ context.Context, value service.Node) (service.Node, error) {
	return value, nil
}

func (l *Laboratory) Open(_ context.Context, initial int64) (types.CounterHandler, types.Details, error) {
	c := &counter{owner: l}
	// The conformance fixture reserves MinInt64 for one failed cleanup attempt.
	c.failClose.Store(initial == math.MinInt64)
	c.value.Store(initial)
	return c, types.Details{Label: "counter", Mode: 0}, nil
}

func (l *Laboratory) Read(ctx context.Context, value types.CounterHandler) (int64, error) {
	return value.Add(ctx, 0)
}
func (l *Laboratory) Wait(ctx context.Context) (int64, error) { <-ctx.Done(); return 0, ctx.Err() }

type Report struct {
	Echo      int   `json:"echo"`
	Recursive bool  `json:"recursive"`
	Resource  int64 `json:"resource"`
	Canceled  bool  `json:"canceled"`
}

// ExerciseResourceRetry verifies remote cleanup ownership after a business error.
func ExerciseResourceRetry(ctx context.Context, binder rpc.Binder) error {
	client, err := service.BindLaboratoryClient(ctx, binder, rpc.BindOptions{})
	if err != nil {
		return err
	}
	defer client.Close()
	resource, _, err := client.Open(ctx, math.MinInt64)
	if err != nil {
		return err
	}
	err = resource.Close(ctx)
	if code, _ := rpc.CodeOf(err); code != rpc.CodeFailedPrecondition {
		return fmt.Errorf("first resource close: %v", err)
	}
	if _, err := resource.Add(ctx, 0); err == nil {
		return errors.New("closing resource still accepts calls")
	}
	if err := resource.Close(ctx); err != nil {
		return err
	}
	return resource.Close(ctx)
}

// Exercise verifies observable typed results; both peer directions run this contract.
func Exercise(ctx context.Context, binder rpc.Binder) (Report, error) {
	var report Report
	client, err := service.BindLaboratoryClient(ctx, binder, rpc.BindOptions{})
	if err != nil {
		return report, fmt.Errorf("bind Laboratory client: %w", err)
	}
	defer client.Close()
	for _, data := range [][]byte{nil, {}, {0, 255}} {
		packet := service.Packet{Data: data, Values: []int64{-1, 9223372036854775807}, Labels: map[string]string{"b": "2", "a": "1"}, Optional_data: &data, Details: types.Details{Label: strings.Repeat("中", 512), Mode: 99}}
		packet.Scalars = &types.Scalars{I8: math.MinInt8, I16: math.MinInt16, I32: math.MinInt32, I64: math.MinInt64, U8: math.MaxUint8, U16: math.MaxUint16, U32: math.MaxUint32, U64: math.MaxUint64, F32: math.Float32frombits(0x80000000), F64: math.Float64frombits(1), C64: complex(float32(math.Inf(1)), float32(math.Inf(-1))), C128: complex(math.Float64frombits(1), math.Copysign(0, -1)), Flags: map[bool]string{false: "no", true: "yes"}, Numbers: map[int64]string{math.MinInt64: "low", math.MaxInt64: "high"}, Type: "keyword"}
		got, err := client.Echo(ctx, packet)
		if err != nil {
			return report, fmt.Errorf("Echo: %w", err)
		}
		if !reflect.DeepEqual(got, packet) {
			return report, errors.New("packet round trip mismatch")
		}
		if math.Float32bits(got.Scalars.F32) != 0x80000000 || math.Float64bits(imag(got.Scalars.C128)) != 1<<63 {
			return report, errors.New("floating bit pattern mismatch")
		}
		report.Echo++
	}
	node := service.Node{Value: 1, Next: &service.Node{Value: 2}}
	got, err := client.Tree(ctx, node)
	if err != nil {
		return report, fmt.Errorf("Tree: %w", err)
	}
	if !reflect.DeepEqual(got, node) {
		return report, errors.New("recursive message mismatch")
	}
	report.Recursive = true
	resource, details, err := client.Open(ctx, 40)
	if err != nil {
		return report, fmt.Errorf("Open: %w", err)
	}
	if details.Label != "counter" {
		return report, errors.New("resource metadata mismatch")
	}
	value, err := resource.Add(ctx, 2)
	if err != nil {
		return report, fmt.Errorf("counter Add: %w", err)
	}
	if value != 42 {
		return report, fmt.Errorf("resource value %d", value)
	}
	report.Resource, err = client.Read(ctx, resource)
	if err != nil {
		return report, fmt.Errorf("Read: %w", err)
	}
	if report.Resource != 42 {
		return report, errors.New("resource argument mismatch")
	}
	// A closed service client drains its route; the resource still owns its lease.
	if err := client.Close(); err != nil {
		return report, err
	}
	if value, err := resource.Add(ctx, 0); err != nil || value != 42 {
		return report, fmt.Errorf("retained resource: %d, %w", value, err)
	}
	if err := resource.Close(ctx); err != nil {
		return report, err
	}
	client, err = service.BindLaboratoryClient(ctx, binder, rpc.BindOptions{})
	if err != nil {
		return report, err
	}
	defer client.Close()
	wait, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = client.Wait(wait)
	code, _ := rpc.CodeOf(err)
	if !errors.Is(err, context.DeadlineExceeded) && code != rpc.CodeDeadlineExceeded {
		return report, fmt.Errorf("Wait cancellation: %w", err)
	}
	report.Canceled = true
	return report, client.Close()
}
