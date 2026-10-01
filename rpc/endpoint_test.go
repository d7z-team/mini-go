package rpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestEndpointSupportsSimultaneousBidirectionalBindings(t *testing.T) {
	methodA := Method{ID: "example/a::Service.Value", Service: "example/a::Service", Name: "Value", ContractHash: testContractHash}
	methodB := Method{ID: "example/b::Service.Value", Service: "example/b::Service", Name: "Value", ContractHash: testContractHash}
	gatewayA := newTestBinder(testBinderOptions{})
	gatewayB := newTestBinder(testBinderOptions{})
	if err := gatewayA.Register(newTestProvider(t, methodA, func(context.Context, []Value) ([]Value, error) {
		return []Value{{Type: "string", Data: "a"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := gatewayB.Register(newTestProvider(t, methodB, func(context.Context, []Value) ([]Value, error) {
		return []Value{{Type: "string", Data: "b"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	endpointA, endpointB := openEndpointPair(t, gatewayA, gatewayB, EndpointOptions{})
	type binding struct {
		routes *RouteSet
		err    error
	}
	boundA, boundB := make(chan binding, 1), make(chan binding, 1)
	go func() {
		routes, bindErr := endpointA.Bind(context.Background(), testBindRequest(methodB))
		boundA <- binding{routes: routes, err: bindErr}
	}()
	go func() {
		routes, bindErr := endpointB.Bind(context.Background(), testBindRequest(methodA))
		boundB <- binding{routes: routes, err: bindErr}
	}()
	a, b := <-boundA, <-boundB
	if a.err != nil || b.err != nil {
		t.Fatalf("bidirectional bind: a=%v b=%v", a.err, b.err)
	}
	defer a.routes.Close()
	defer b.routes.Close()
	resultA, err := a.routes.Call(context.Background(), Call{Method: methodB})
	if err != nil {
		t.Fatal(err)
	}
	defer resultA.Discard(context.Background())
	resultB, err := b.routes.Call(context.Background(), Call{Method: methodA})
	if err != nil {
		t.Fatal(err)
	}
	defer resultB.Discard(context.Background())
	if resultA.Values[0].Data != "b" || resultB.Values[0].Data != "a" {
		t.Fatalf("bidirectional results: a=%#v b=%#v", resultA.Values, resultB.Values)
	}
}

func TestEndpointRejectsResponseWithDifferentKind(t *testing.T) {
	limits := normalizeLimits(Limits{})
	endpoint := &Endpoint{
		options: EndpointOptions{LeaseTTL: time.Second, AdmissionTimeout: time.Second},
		state:   endpointOpen, origin: "local", limits: limits, outboundLimits: limits,
		pending:     make(map[uint64]endpointPending),
		replyWrites: make(chan endpointWrite, 1), requestWrites: make(chan endpointWrite, 1),
		helloSent: make(chan struct{}), stopping: make(chan struct{}),
		requestGate: make(chan struct{}, 1),
	}
	close(endpoint.helloSent)
	done := make(chan error, 1)
	go func() {
		_, err := endpoint.request(context.Background(), endpointFrame{
			Kind: "call", Binding: 1,
			Call: &wireCall{Method: Method{ID: "example/service::Service.Value", Service: "example/service::Service", Name: "Value", ContractHash: testContractHash}},
		})
		done <- err
	}()
	request := <-endpoint.requestWrites
	endpoint.releaseQueuedWrite(len(request.payload))
	if request.done != nil {
		request.done <- nil
	}
	requestFrame, err := decodeEndpointFrame(request.payload, limits)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.mu.Lock()
	pending := endpoint.pending[requestFrame.ID]
	endpoint.removePendingLocked(requestFrame.ID)
	endpoint.mu.Unlock()
	pending.response <- endpointFrame{Kind: "bind", Reply: true, TargetID: requestFrame.ID}
	if err := <-done; codeOf(err) != CodeProtocol {
		t.Fatalf("mismatched response kind error = %v", err)
	}
}

func TestEndpointPropagatesCancellation(t *testing.T) {
	method := Method{ID: "example/cancel::Service.Wait", Service: "example/cancel::Service", Name: "Wait", ContractHash: testContractHash}
	started := make(chan struct{})
	var canceled atomic.Bool
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(newTestProvider(t, method, func(ctx context.Context, _ []Value) ([]Value, error) {
		close(started)
		<-ctx.Done()
		canceled.Store(true)
		return nil, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	client, server := openEndpointPair(t, nil, gateway, EndpointOptions{})
	routes, err := client.Bind(context.Background(), testBindRequest(method))
	if err != nil {
		t.Fatalf("bind: %v (client=%v server=%v)", err, client.endpointError(), server.endpointError())
	}
	defer routes.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := routes.Call(ctx, Call{Method: method})
		done <- callErr
	}()
	<-started
	cancel()
	callErr := <-done
	code, _ := CodeOf(callErr)
	if code != CodeCanceled {
		t.Fatalf("canceled call error = %v", callErr)
	}
	deadline := time.Now().Add(time.Second)
	for !canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !canceled.Load() {
		t.Fatal("remote handler did not observe cancellation")
	}
}

func TestEndpointAppliesMaxCallDuration(t *testing.T) {
	method := Method{ID: "example/timeout::Service.Wait", Service: "example/timeout::Service", Name: "Wait", ContractHash: testContractHash}
	canceled := make(chan struct{})
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(newTestProvider(t, method, func(ctx context.Context, _ []Value) ([]Value, error) {
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	client, _ := openEndpointPair(t, nil, gateway, EndpointOptions{MaxCallDuration: 10 * time.Millisecond})
	routes, err := client.Bind(context.Background(), testBindRequest(method))
	if err != nil {
		t.Fatal(err)
	}
	defer routes.Close()
	if _, err := routes.Call(context.Background(), Call{Method: method}); codeOf(err) != CodeDeadlineExceeded {
		t.Fatalf("timed call error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("remote handler did not observe its inbound timeout")
	}
}

func TestEndpointBoundsQueuesAndReleasesClosedState(t *testing.T) {
	client, server := openEndpointPair(t, nil, nil, EndpointOptions{})
	if cap(client.replyWrites) >= client.limits.MaxPendingControls || cap(client.requestWrites) >= client.limits.MaxPendingCalls {
		t.Fatalf("endpoint handoff queues track protocol limits: replies=%d requests=%d", cap(client.replyWrites), cap(client.requestWrites))
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Done():
	case <-time.After(time.Second):
		t.Fatal("peer endpoint did not close")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.pending != nil || client.active != nil || client.inbound != nil || client.outbound != nil || client.results != nil ||
		client.replyWrites != nil || client.controlWrites != nil || client.requestWrites != nil {
		t.Fatal("closed endpoint retained request or queue state")
	}
}

func TestEndpointLeaseMaintenanceStopsAfterShutdown(t *testing.T) {
	client, server := openEndpointPair(t, nil, nil, EndpointOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Done():
	case <-ctx.Done():
		t.Fatal("peer endpoint did not close")
	}
	// A goroutine started by Ready may first run after shutdown has completed.
	server.maintainLeases()
}

func TestEndpointWaitKeepsCleanupErrorSeparateFromTransport(t *testing.T) {
	want := errors.New("lease cleanup failed")
	lease := &blockingBindingLease{started: make(chan struct{}), release: make(chan struct{}), err: want}
	method := Method{ID: "wait::Service.Call", Service: "wait::Service", Name: "Call", ContractHash: testContractHash}
	binder, err := NewLocalBinder(LocalBinderOptions{}, localBinderProvider{
		contract: testContract(method), bind: func(context.Context, BindRequest) (ProviderLease, error) { return lease, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, server := openEndpointPair(t, nil, binder, EndpointOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := client.Bind(ctx, testBindRequest(method)); err != nil {
		t.Fatal(err)
	}
	wait, stop := context.WithCancel(ctx)
	stop()
	if err := server.Wait(wait); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := server.endpointError(); err != nil {
		t.Fatal("wait closed transport", err)
	}
	_ = client.Close()
	select {
	case <-lease.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := server.Wait(wait); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(lease.release)
	if err := server.Wait(ctx); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if err := client.Wait(ctx); err != nil {
		t.Fatal("transport reason leaked into Wait", err)
	}
}

func TestEndpointEnforcesNegotiatedBindingLimit(t *testing.T) {
	method := Method{ID: "example/bindings::Service.Value", Service: "example/bindings::Service", Name: "Value", ContractHash: testContractHash}
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(newTestProvider(t, method, func(context.Context, []Value) ([]Value, error) { return nil, nil })); err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := newTestMessagePipe()
	client, err := OpenEndpoint(clientConn, EndpointServices{}, EndpointOptions{Limits: Limits{MaxBindings: 1}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := OpenEndpoint(serverConn, EndpointServices{Binder: gateway}, EndpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()
	first, err := client.Bind(context.Background(), testBindRequest(method))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := client.Bind(context.Background(), testBindRequest(method)); codeOf(err) != CodeResourceExhausted {
		t.Fatalf("second binding error = %v", err)
	}
}

func TestEndpointRequestContextDoesNotWaitForBlockedMessageWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		method := Method{ID: "example/write::Service.Value", Service: "example/write::Service", Name: "Value", ContractHash: testContractHash}
		clientSide, serverSide := newTestMessagePipe()
		clientConn := &blockedWriteConn{MessageConn: clientSide, release: make(chan struct{}), entered: make(chan struct{}, 1)}
		client, err := OpenEndpoint(clientConn, EndpointServices{}, EndpointOptions{})
		if err != nil {
			t.Fatal(err)
		}
		server, err := OpenEndpoint(serverSide, EndpointServices{}, EndpointOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		defer server.Close()
		if err := client.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		clientConn.blocked.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, bindErr := client.Bind(ctx, testBindRequest(method)); done <- bindErr }()
		<-clientConn.entered
		cancel()
		err = <-done
		if codeOf(err) != CodeCanceled {
			t.Fatalf("blocked write error = %v", err)
		}
		select {
		case <-clientConn.release:
			t.Fatal("transport released before request cancellation returned")
		default:
		}
	})
}

func TestEndpointCancellationReleasesLateBinding(t *testing.T) {
	method := Method{ID: "example/cancel::Service.Bind", Service: "example/cancel::Service", Name: "Bind", ContractHash: testContractHash}
	provider := &delayedBindProvider{method: method, bound: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(provider); err != nil {
		t.Fatal(err)
	}
	client, _ := openEndpointPair(t, nil, gateway, EndpointOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, bindErr := client.Bind(ctx, testBindRequest(method))
		done <- bindErr
	}()
	<-provider.bound
	cancel()
	if bindErr := <-done; bindErr == nil {
		t.Fatal("canceled bind succeeded")
	}
	close(provider.release)
	select {
	case <-provider.closed:
	case <-time.After(time.Second):
		t.Fatal("provider lease from canceled bind was not closed")
	}
}

func TestEndpointCloseReleasesLateBinding(t *testing.T) {
	method := Method{ID: "example/close::Service.Bind", Service: "example/close::Service", Name: "Bind", ContractHash: testContractHash}
	provider := &delayedBindProvider{method: method, bound: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(provider); err != nil {
		t.Fatal(err)
	}
	client, server := openEndpointPair(t, nil, gateway, EndpointOptions{})
	done := make(chan error, 1)
	go func() {
		_, bindErr := client.Bind(context.Background(), testBindRequest(method))
		done <- bindErr
	}()
	<-provider.bound
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-done; codeOf(err) != CodeUnavailable {
		t.Fatalf("bind after endpoint close = %v", err)
	}
	select {
	case <-provider.closed:
	case <-time.After(time.Second):
		t.Fatal("provider lease from late bind was not closed")
	}
	select {
	case <-server.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("endpoint shutdown did not finish")
	}
}

func TestEndpointCloseDiscardsLateCallResult(t *testing.T) {
	method := Method{ID: "example/close::Service.Wait", Service: "example/close::Service", Name: "Wait", ContractHash: testContractHash}
	provider := &delayedCallProvider{method: method, started: make(chan struct{}), release: make(chan struct{}), decision: make(chan bool, 1)}
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(provider); err != nil {
		t.Fatal(err)
	}
	client, server := openEndpointPair(t, nil, gateway, EndpointOptions{})
	routes, err := client.Bind(context.Background(), testBindRequest(method))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, callErr := routes.Call(context.Background(), Call{Method: method})
		done <- callErr
	}()
	<-provider.started
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-done; codeOf(err) != CodeUnavailable {
		t.Fatalf("call after endpoint close = %v", err)
	}
	select {
	case accepted := <-provider.decision:
		if accepted {
			t.Fatal("late result was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("late result was not discarded")
	}
	select {
	case <-server.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("endpoint shutdown did not finish")
	}
	if provider.closes.Load() != 1 {
		t.Fatalf("provider lease closes = %d", provider.closes.Load())
	}
}

func TestEndpointCloseFromHandlerDoesNotWaitForItsDispatch(t *testing.T) {
	method := Method{ID: "example/self-close::Service.Close", Service: "example/self-close::Service", Name: "Close", ContractHash: testContractHash}
	var server *Endpoint
	gateway := newTestBinder(testBinderOptions{})
	if err := gateway.Register(newTestProvider(t, method, func(context.Context, []Value) ([]Value, error) {
		if err := server.Close(); err != nil {
			return nil, err
		}
		return nil, nil
	})); err != nil {
		t.Fatal(err)
	}
	client, openedServer := openEndpointPair(t, nil, gateway, EndpointOptions{})
	server = openedServer
	routes, err := client.Bind(context.Background(), testBindRequest(method))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, callErr := routes.Call(context.Background(), Call{Method: method})
		done <- callErr
	}()
	select {
	case err := <-done:
		if codeOf(err) != CodeUnavailable {
			t.Fatalf("self-closing handler call = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Endpoint.Close waited for its own dispatch")
	}
	select {
	case <-server.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("endpoint shutdown did not finish")
	}
}
