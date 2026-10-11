package rpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testContractHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testResourceHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func encodeEndpointFragment(messageID uint64, total, offset int, data []byte, maxFrameBytes int) ([]byte, error) {
	return encodeEndpointFragmentTo(nil, messageID, total, offset, data, maxFrameBytes)
}

func encodeFFIRequest(request ffiRequest) []byte {
	var encoder wireEncoder
	encoder.String(request.Version)
	encoder.String(request.Operation)
	encoder.String(request.Target)
	encoder.Uint(request.Lease)
	encoder.String(request.Contract.Protocol)
	encoder.Uint(uint64(len(request.Contract.Methods)))
	for _, method := range request.Contract.Methods {
		encodeWireMethod(&encoder, method)
	}
	encoder.String(request.Options.AffinityKey)
	encodeWireLabels(&encoder, request.Options.Labels)
	encodeWireMethod(&encoder, request.Method)
	encodeWireResource(&encoder, request.Receiver)
	encoder.Raw(request.Payload)
	encodeWireResource(&encoder, request.Resource)
	encoder.Uint(request.RequestID)
	encoder.String(string(request.Code))
	encoder.String(request.Message)
	encoder.Int(request.TimeoutNanos)
	return encoder.data
}

func decodeFFIResponse(data []byte) (ffiResponse, error) {
	limits := normalizeLimits(Limits{})
	if len(data) > limits.MaxMessageBytes {
		return ffiResponse{}, errors.New("MRPC FFI response exceeds message limit")
	}
	decoder := newWireDecoder(data, limits.MaxMessageBytes)
	response := ffiResponse{}
	var err error
	response.Version, err = decoder.String()
	if err == nil {
		response.Operation, err = decoder.String()
	}
	if err == nil {
		response.Lease, err = decoder.Uint()
	}
	if err == nil {
		response.RequestID, err = decoder.Uint()
	}
	if err == nil {
		response.Method, err = decodeWireMethod(decoder)
	}
	if err == nil {
		response.Receiver, err = decodeWireResource(decoder)
	}
	if err == nil {
		response.Payload, err = decoder.Raw()
	}
	var code string
	if err == nil {
		code, err = decoder.String()
		response.Code = Code(code)
	}
	if err == nil {
		response.Message, err = decoder.String()
	}
	if err == nil {
		err = decoder.Done()
	}
	return response, err
}

type blockingBindingLease struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (*blockingBindingLease) Invoke(context.Context, Method, []Value) (*ProviderResult, error) {
	return &ProviderResult{}, nil
}

func (l *blockingBindingLease) Close() error {
	close(l.started)
	<-l.release
	return l.err
}

type orderedResource struct {
	Resource
	id    int
	order *[]int
}

func (r *orderedResource) Close(ctx context.Context) error {
	*r.order = append(*r.order, r.id)
	return r.Resource.Close(ctx)
}

func newTestProvider(t *testing.T, method Method, invoke MethodHandler) Provider {
	t.Helper()
	provider, err := NewProvider(MethodBinding{Method: method, Invoke: invoke})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func testContract(methods ...Method) Contract {
	return Contract{Protocol: ContractProtocol, Methods: methods}
}

func testBindRequest(methods ...Method) BindRequest {
	return BindRequest{Contract: testContract(methods...)}
}

func codeOf(err error) Code {
	code, _ := CodeOf(err)
	return code
}

type testMessageConn struct {
	incoming <-chan []byte
	outgoing chan<- []byte
	closed   chan struct{}
	peerDone <-chan struct{}
	once     sync.Once
}

func newTestMessagePipe() (*testMessageConn, *testMessageConn) {
	leftToRight := make(chan []byte, 16)
	rightToLeft := make(chan []byte, 16)
	leftDone := make(chan struct{})
	rightDone := make(chan struct{})
	return &testMessageConn{incoming: rightToLeft, outgoing: leftToRight, closed: leftDone, peerDone: rightDone},
		&testMessageConn{incoming: leftToRight, outgoing: rightToLeft, closed: rightDone, peerDone: leftDone}
}

func (conn *testMessageConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case message := <-conn.incoming:
		return append([]byte(nil), message...), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-conn.closed:
		return nil, io.ErrClosedPipe
	case <-conn.peerDone:
		return nil, io.EOF
	}
}

func (conn *testMessageConn) Write(ctx context.Context, message []byte) error {
	message = append([]byte(nil), message...)
	select {
	case conn.outgoing <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.closed:
		return io.ErrClosedPipe
	case <-conn.peerDone:
		return io.ErrClosedPipe
	}
}

func (conn *testMessageConn) Close() error {
	conn.once.Do(func() { close(conn.closed) })
	return nil
}

func openEndpointPair(t *testing.T, clientBinder, serverBinder Binder, serverOptions EndpointOptions) (*Endpoint, *Endpoint) {
	t.Helper()
	clientConn, serverConn := newTestMessagePipe()
	client, err := OpenEndpoint(clientConn, EndpointServices{Binder: clientBinder}, EndpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server, err := OpenEndpoint(serverConn, EndpointServices{Binder: serverBinder}, serverOptions)
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		for _, endpoint := range []*Endpoint{client, server} {
			select {
			case <-endpoint.shutdownDone:
			case <-time.After(time.Second):
				t.Error("endpoint cleanup did not finish")
			}
		}
	})
	return client, server
}

type testBinderOptions struct {
	Limits     Limits
	Authorizer func(context.Context, PeerInfo, Contract) error
}

type testBinder struct {
	options   testBinderOptions
	providers []Provider
}

func newTestBinder(options testBinderOptions) *testBinder { return &testBinder{options: options} }

func (b *testBinder) Register(provider Provider) error {
	if provider == nil {
		return errors.New("test binder provider is nil")
	}
	b.providers = append(b.providers, provider)
	return nil
}

func (b *testBinder) Bind(ctx context.Context, request BindRequest) (*RouteSet, error) {
	if b.options.Authorizer != nil {
		if err := b.options.Authorizer(ctx, request.Peer, request.Contract); err != nil {
			return nil, err
		}
	}
	binder, err := NewLocalBinder(LocalBinderOptions{Limits: b.options.Limits}, b.providers...)
	if err != nil {
		return nil, err
	}
	return binder.Bind(ctx, request)
}

type testResource struct{ closed bool }

func (r *testResource) Invoke(context.Context, string, []Value) ([]Value, error) {
	if r.closed {
		return nil, StatusError{Code: CodeNotFound, Message: "closed"}
	}
	return []Value{{Type: "string", Data: "value"}}, nil
}

func (r *testResource) Close(context.Context) error {
	if r.closed {
		return errors.New("resource closed twice")
	}
	r.closed = true
	return nil
}

type countedResource struct {
	closes atomic.Int32
}

func (*countedResource) Invoke(context.Context, string, []Value) ([]Value, error) {
	return nil, nil
}

func (r *countedResource) Close(context.Context) error {
	r.closes.Add(1)
	return nil
}

type notifyingProviderLease struct {
	closes atomic.Int32
	done   chan struct{}
}

func (l *notifyingProviderLease) Invoke(context.Context, Method, []Value) (*ProviderResult, error) {
	return &ProviderResult{}, nil
}

func (l *notifyingProviderLease) Close() error {
	if l.closes.Add(1) == 1 {
		close(l.done)
	}
	return nil
}
