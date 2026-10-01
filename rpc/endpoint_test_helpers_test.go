package rpc

import (
	"context"
	"sync"
	"sync/atomic"
)

type blockedWriteConn struct {
	MessageConn
	blocked atomic.Bool
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (conn *blockedWriteConn) Write(ctx context.Context, message []byte) error {
	if conn.blocked.Load() {
		if conn.entered != nil {
			select {
			case conn.entered <- struct{}{}:
			default:
			}
		}
		select {
		case <-conn.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return conn.MessageConn.Write(ctx, message)
}

func (conn *blockedWriteConn) Close() error {
	conn.once.Do(func() { close(conn.release) })
	return conn.MessageConn.Close()
}

type delayedBindProvider struct {
	method  Method
	bound   chan struct{}
	release chan struct{}
	closed  chan struct{}
}

func (p *delayedBindProvider) RPCContract() Contract { return testContract(p.method) }

func (p *delayedBindProvider) BindRPC(context.Context, BindRequest) (ProviderLease, error) {
	close(p.bound)
	<-p.release
	return &delayedProviderLease{closed: p.closed}, nil
}

type delayedProviderLease struct {
	closed chan struct{}
	once   sync.Once
}

func (l *delayedProviderLease) Invoke(context.Context, Method, []Value) (*ProviderResult, error) {
	return &ProviderResult{}, nil
}

func (l *delayedProviderLease) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

type delayedCallProvider struct {
	method   Method
	started  chan struct{}
	release  chan struct{}
	decision chan bool
	closes   atomic.Int32
}

func (p *delayedCallProvider) RPCContract() Contract { return testContract(p.method) }

func (p *delayedCallProvider) BindRPC(context.Context, BindRequest) (ProviderLease, error) {
	return &delayedCallLease{provider: p}, nil
}

type delayedCallLease struct {
	provider *delayedCallProvider
	once     sync.Once
}

func (l *delayedCallLease) Invoke(context.Context, Method, []Value) (*ProviderResult, error) {
	l.once.Do(func() { close(l.provider.started) })
	<-l.provider.release
	return &ProviderResult{Values: []Value{{Type: "string", Data: "late"}}, decide: func(accept bool) error {
		l.provider.decision <- accept
		return nil
	}}, nil
}

func (l *delayedCallLease) Close() error {
	l.provider.closes.Add(1)
	return nil
}
