package rpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestRouteSetShutdownRetainsBorrowUntilHandlerReturns(t *testing.T) {
	resource := &borrowGateResource{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), selfErr: make(chan error, 1)}
	routes, read, ref := bindBorrowResource(t, resource)
	resource.resolveRoutes, resource.resolveRef = routes, ref
	callDone := make(chan error, 1)
	go func() {
		result, err := routes.Call(t.Context(), Call{Method: read, Receiver: &ref})
		if result != nil {
			_ = result.Discard(context.Background())
		}
		callDone <- err
	}()
	<-resource.started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := routes.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-resource.closed:
		t.Fatal("shutdown closed resource before handler returned")
	default:
	}
	close(resource.release)
	if err := <-resource.selfErr; err != nil {
		t.Fatalf("borrow lost during shutdown: %v", err)
	}
	<-callDone
	if err := routes.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resource.closes.Load() != 1 {
		t.Fatal("shutdown did not release resource exactly once")
	}
}

type borrowGateResource struct {
	savedContext  context.Context
	started       chan struct{}
	release       chan struct{}
	closed        chan struct{}
	closes        atomic.Int32
	handle        *ResourceHandle
	selfErr       chan error
	otherRoutes   *RouteSet
	otherRef      ResourceRef
	resolveRoutes *RouteSet
	resolveRef    ResourceRef
	panic         bool
}

func (r *borrowGateResource) Invoke(ctx context.Context, _ string, _ []Value) ([]Value, error) {
	r.savedContext = ctx
	if r.panic {
		panic("resource invocation panic")
	}
	if r.handle != nil {
		r.selfErr <- r.handle.Close(ctx)
		return nil, nil
	}
	if r.otherRoutes != nil {
		r.selfErr <- r.otherRoutes.Drop(ctx, r.otherRef)
		return nil, nil
	}
	close(r.started)
	<-r.release
	if r.resolveRoutes != nil {
		_, err := Resolve(ctx, r.resolveRef)
		r.selfErr <- err
	}
	return nil, nil
}

func TestRouteSetBorrowScopeEndsWithInvocation(t *testing.T) {
	resource := &borrowGateResource{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	close(resource.release)
	routes, read, ref := bindBorrowResource(t, resource)
	t.Cleanup(func() { _ = routes.Shutdown(context.Background()) })
	result, err := routes.Call(t.Context(), Call{Method: read, Receiver: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Discard(t.Context()); err != nil {
		t.Fatal(err)
	}
	retained := context.WithoutCancel(resource.savedContext)
	if _, err := Resolve(retained, ref); codeOf(err) != CodeFailedPrecondition {
		t.Fatalf("finished invocation kept borrow: %v", err)
	}
	if err := routes.Drop(retained, ref); err != nil {
		t.Fatalf("finished invocation was mistaken for self-close: %v", err)
	}
}

func (r *borrowGateResource) Close(context.Context) error {
	r.closes.Add(1)
	close(r.closed)
	return nil
}

func bindBorrowResource(t *testing.T, resource Resource) (*RouteSet, Method, ResourceRef) {
	t.Helper()
	open := Method{ID: "borrow::Files.Open", Service: "borrow::Files", Name: "Open", ContractHash: testContractHash}
	read := Method{ID: "borrow::File.Read", Service: "borrow::File", Name: "Read", ContractHash: testContractHash, ResourceTypeHash: testResourceHash}
	provider, err := NewProvider(
		MethodBinding{Method: open, Invoke: func(ctx context.Context, _ []Value) ([]Value, error) {
			value, exportErr := Export(ctx, resource, testResourceHash)
			return []Value{value}, exportErr
		}},
		MethodBinding{Method: read},
	)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewLocalBinder(LocalBinderOptions{}, provider)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := binder.Bind(context.Background(), testBindRequest(open, read))
	if err != nil {
		t.Fatal(err)
	}
	result, err := routes.Call(context.Background(), Call{Method: open})
	if err != nil {
		routes.Shutdown(context.Background())
		t.Fatal(err)
	}
	if err := result.Accept(context.Background()); err != nil {
		routes.Shutdown(context.Background())
		t.Fatal(err)
	}
	return routes, read, *result.Values[0].Resource
}

func TestRouteSetBorrowsReceiverAndArgumentsAtomicallyUntilInvokeReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resource := &borrowGateResource{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
		routes, read, ref := bindBorrowResource(t, resource)
		defer routes.Shutdown(context.Background())

		callDone := make(chan error, 1)
		go func() {
			_, err := routes.Call(context.Background(), Call{Method: read, Receiver: &ref, Arguments: []Value{{Type: ref.TypeHash, Resource: &ref}}})
			callDone <- err
		}()
		select {
		case <-resource.started:
		case <-time.After(time.Second):
			t.Fatal("resource invocation did not start")
		}
		routes.mu.Lock()
		entry := routes.resources[ref.ObjectID]
		if entry == nil || entry.borrowed != 1 {
			routes.mu.Unlock()
			t.Fatalf("borrow count = %v", entry)
		}
		routes.mu.Unlock()

		closeDone := make(chan error, 1)
		go func() { closeDone <- routes.Drop(context.Background(), ref) }()
		synctest.Wait()
		select {
		case <-resource.closed:
			t.Fatal("resource closed while invocation was still running")
		default:
		}
		select {
		case err := <-closeDone:
			t.Fatalf("close returned before invocation: %v", err)
		default:
		}
		close(resource.release)
		if err := <-callDone; err != nil {
			t.Fatal(err)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if resource.closes.Load() != 1 {
			t.Fatalf("close count = %d", resource.closes.Load())
		}
	})
}

func TestRouteSetRejectsSynchronousResourceSelfClose(t *testing.T) {
	resource := &borrowGateResource{selfErr: make(chan error, 1), closed: make(chan struct{})}
	routes, read, ref := bindBorrowResource(t, resource)
	resource.handle, _ = routes.BindResource(ref)
	defer routes.Shutdown(context.Background())

	result, err := routes.Call(context.Background(), Call{Method: read, Receiver: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Discard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-resource.selfErr; codeOf(err) != CodeFailedPrecondition {
		t.Fatalf("self close error = %v", err)
	}
	if err := routes.Drop(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestRouteSetReleasesBorrowAfterInvocationPanic(t *testing.T) {
	resource := &borrowGateResource{panic: true, closed: make(chan struct{})}
	routes, read, ref := bindBorrowResource(t, resource)
	defer routes.Shutdown(context.Background())

	if _, err := routes.Call(context.Background(), Call{Method: read, Receiver: &ref}); codeOf(err) != CodeInternal {
		t.Fatalf("panic call error = %v", err)
	}
	if err := routes.Drop(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if resource.closes.Load() != 1 {
		t.Fatalf("close count = %d", resource.closes.Load())
	}
}

func TestRouteSetBorrowScopeDoesNotCrossRouteSets(t *testing.T) {
	resource := &borrowGateResource{selfErr: make(chan error, 1), closed: make(chan struct{})}
	routes, read, ref := bindBorrowResource(t, resource)
	otherResource := &countedResource{}
	other := newRouteSet(ref.Epoch, normalizeLimits(Limits{}), nil, nil)
	other.resources[ref.ObjectID] = &providerResource{resource: otherResource, typeHash: ref.TypeHash, active: true}
	other.resourceOrder = append(other.resourceOrder, ref.ObjectID)
	resource.otherRoutes, resource.otherRef = other, ref
	defer routes.Shutdown(context.Background())
	defer other.Shutdown(context.Background())

	result, err := routes.Call(context.Background(), Call{Method: read, Receiver: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Discard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-resource.selfErr; err != nil {
		t.Fatalf("cross-route close failed: %v", err)
	}
	if otherResource.closes.Load() != 1 {
		t.Fatalf("other route close count = %d", otherResource.closes.Load())
	}
}

func TestRouteSetBorrowKeepsResolveValidWhileCloseIsWaiting(t *testing.T) {
	resource := &borrowGateResource{started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), selfErr: make(chan error, 1)}
	routes, read, ref := bindBorrowResource(t, resource)
	resource.resolveRoutes, resource.resolveRef = routes, ref
	defer routes.Shutdown(context.Background())

	callDone := make(chan error, 1)
	go func() {
		_, err := routes.Call(context.Background(), Call{Method: read, Receiver: &ref})
		callDone <- err
	}()
	select {
	case <-resource.started:
	case <-time.After(time.Second):
		t.Fatal("resource invocation did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- routes.Drop(context.Background(), ref) }()
	deadline := time.Now().Add(time.Second)
	for {
		routes.mu.Lock()
		closing := routes.resourceCloses[ref.ObjectID] != nil
		routes.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resource close did not become pending")
		}
		time.Sleep(time.Millisecond)
	}
	close(resource.release)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resource.selfErr; err != nil {
		t.Fatalf("borrowed Resolve failed during Close: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}
