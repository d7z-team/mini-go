package rpc

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type renewableWorkLease struct {
	callStarted, callRelease         chan struct{}
	decisionStarted, decisionRelease chan struct{}
	resource                         *gatedCloseResource
}

func (l *renewableWorkLease) Invoke(ctx context.Context, _ Method, _ []Value) (*ProviderResult, error) {
	value, err := Export(ctx, l.resource, testResourceHash)
	if err != nil {
		return nil, err
	}
	close(l.callStarted)
	<-l.callRelease
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return NewProviderResult([]Value{value}, func(bool) error {
		close(l.decisionStarted)
		<-l.decisionRelease
		return nil
	}), nil
}

func (*renewableWorkLease) Close() error { return nil }

func TestEndpointRenewalKeepsCallDecisionAndCloseAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ttl = 300 * time.Millisecond
		resource := &gatedCloseResource{started: make(chan struct{}), finish: make(chan struct{})}
		lease := &renewableWorkLease{callStarted: make(chan struct{}), callRelease: make(chan struct{}), decisionStarted: make(chan struct{}), decisionRelease: make(chan struct{}), resource: resource}
		releaseCall := sync.OnceFunc(func() { close(lease.callRelease) })
		releaseDecision := sync.OnceFunc(func() { close(lease.decisionRelease) })
		releaseClose := sync.OnceFunc(func() { close(resource.finish) })
		method := Method{ID: "lease::Service.Open", Service: "lease::Service", Name: "Open", ContractHash: testContractHash}
		binder, err := NewLocalBinder(LocalBinderOptions{}, localBinderProvider{contract: testContract(method), bind: func(context.Context, BindRequest) (ProviderLease, error) { return lease, nil }})
		if err != nil {
			t.Fatal(err)
		}
		client, server := openEndpointPair(t, nil, binder, EndpointOptions{LeaseTTL: ttl, AdmissionTimeout: 100 * time.Millisecond})
		t.Cleanup(func() {
			releaseCall()
			releaseDecision()
			releaseClose()
			_ = client.Close()
			_ = server.Close()
			_ = client.Shutdown(context.Background())
			_ = server.Shutdown(context.Background())
		})
		routes, err := client.Bind(t.Context(), testBindRequest(method))
		if err != nil {
			t.Fatal(err)
		}
		called := make(chan *Result, 1)
		errors := make(chan error, 3)
		go func() { result, err := routes.Call(t.Context(), Call{Method: method}); called <- result; errors <- err }()
		<-lease.callStarted
		time.Sleep(2 * ttl)
		if err := client.endpointError(); err != nil {
			t.Fatal(err)
		}
		releaseCall()
		result := <-called
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		go func() { errors <- result.Accept(t.Context()) }()
		<-lease.decisionStarted
		time.Sleep(2 * ttl)
		if err := client.endpointError(); err != nil {
			t.Fatal(err)
		}
		releaseDecision()
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		go func() { errors <- routes.Drop(t.Context(), *result.Values[0].Resource) }()
		<-resource.started
		time.Sleep(2 * ttl)
		if err := client.endpointError(); err != nil {
			t.Fatal(err)
		}
		releaseClose()
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		if resource.calls.Load() != 1 {
			t.Fatal("close was repeated across renewal rounds")
		}
	})
}

func TestEndpointRenewalAcknowledgementPreservesTargetAndTime(t *testing.T) {
	now := time.Now()
	endpoint := &Endpoint{options: EndpointOptions{LeaseTTL: time.Minute}, outboundLease: map[uint64]time.Time{1: now.Add(10 * time.Second)}, outboundOperations: map[uint64]time.Time{1: now.Add(10 * time.Second)}}
	if err := endpoint.renewOutboundTargets(leaseTargets{}, leaseTargets{}, now, 0); codeOf(err) != CodeProtocol {
		t.Fatalf("zero grant: %v", err)
	}
	if err := endpoint.renewOutboundTargets(leaseTargets{bindings: []uint64{1}}, leaseTargets{operations: []uint64{1}}, now, time.Minute); codeOf(err) != CodeProtocol {
		t.Fatalf("target kind substitution: %v", err)
	}
	for _, sent := range []time.Time{now, now.Add(-2 * time.Minute)} {
		before := endpoint.outboundLease[1]
		if err := endpoint.renewOutboundTargets(leaseTargets{bindings: []uint64{1}}, leaseTargets{bindings: []uint64{1, 2}}, sent, time.Minute); codeOf(err) != CodeProtocol {
			t.Fatalf("unexpected target in ACK sent at %v: %v", sent, err)
		}
		if endpoint.outboundLease[1] != before {
			t.Fatal("invalid ACK partially renewed an owner")
		}
	}
	sent := now.Add(-5 * time.Second)
	if err := endpoint.renewOutboundTargets(leaseTargets{bindings: []uint64{1}}, leaseTargets{bindings: []uint64{1}}, sent, time.Minute); err != nil {
		t.Fatal(err)
	}
	if endpoint.outboundLease[1] != sent.Add(time.Minute) {
		t.Fatal("ACK arrival enlarged authorization")
	}
	if err := endpoint.renewOutboundTargets(leaseTargets{bindings: []uint64{1}}, leaseTargets{bindings: []uint64{1}}, sent.Add(-time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if endpoint.outboundLease[1] != sent.Add(time.Minute) {
		t.Fatal("late older ACK reduced authorization")
	}
	expired := now.Add(-time.Second)
	endpoint.outboundLease[1] = expired
	if err := endpoint.renewOutboundTargets(leaseTargets{bindings: []uint64{1}}, leaseTargets{bindings: []uint64{1}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if endpoint.outboundLease[1] != expired {
		t.Fatal("expired binding revived")
	}
}

func TestEndpointExpiredResultRetainsCapacityWhileOtherOwnersRenew(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	endpoint := &Endpoint{
		limits: normalizeLimits(Limits{}), options: EndpointOptions{LeaseTTL: time.Minute},
		inboundLease:      map[uint64]time.Time{1: time.Now().Add(time.Minute)},
		inboundOperations: make(map[uint64]time.Time),
		results: map[uint64]endpointResult{7: {
			binding: 1, expiresAt: time.Now().Add(-time.Second),
			result: &Result{accept: func(bool) error { close(started); <-release; return nil }},
		}},
	}
	t.Cleanup(func() { close(release); endpoint.dispatchWG.Wait() })
	endpoint.expireLeases()
	<-started
	endpoint.expireLeases()
	ack, err := endpoint.renewInboundTargets(encodeLeaseTargetList([]leaseTarget{{kind: leaseTargetBinding, id: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := decodeLeaseTargets(ack, endpoint.limits)
	if err != nil || len(targets.bindings) != 1 || targets.bindings[0] != 1 {
		t.Fatalf("unrelated owner renewal: %+v %v", targets, err)
	}
	endpoint.mu.Lock()
	entry, present := endpoint.results[7]
	endpoint.mu.Unlock()
	if !present || !entry.retiring {
		t.Fatal("cleanup released result capacity before its owner finished")
	}
}
