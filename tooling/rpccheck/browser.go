package rpccheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/d7z-team/mini-go/rpc"
	service "github.com/d7z-team/mini-go/testdata/rpc/generated/go/service"
)

type browserConnection struct{ socket *websocket.Conn }

func (c browserConnection) Read(ctx context.Context) ([]byte, error) {
	kind, data, err := c.socket.Read(ctx)
	if err == nil && kind != websocket.MessageBinary {
		return nil, errors.New("expected binary RPC frame")
	}
	return data, err
}

func (c browserConnection) Write(ctx context.Context, data []byte) error {
	return c.socket.Write(ctx, websocket.MessageBinary, data)
}
func (c browserConnection) Close() error { return c.socket.CloseNow() }

// RunBrowserPeer serves the shared RPC laboratory and browser test assets on
// the same origin. /exercise validates calls back into the connected guest;
// /disconnect lets lifecycle tests terminate the current transport.
// Returning after a command or input error cancels all owned connections.
func RunBrowserPeer(ctx context.Context, address string, files fs.FS, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	provider, err := service.NewLaboratoryProvider(&Laboratory{})
	if err != nil {
		return err
	}
	binder, err := rpc.NewLocalBinder(rpc.LocalBinderOptions{}, provider)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var current *rpc.Endpoint
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(files)))
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{rpc.EndpointProtocol}})
		if err != nil {
			return
		}
		if conn.Subprotocol() != rpc.EndpointProtocol {
			_ = conn.CloseNow()
			return
		}
		conn.SetReadLimit(1 << 20)
		endpoint, err := rpc.OpenEndpoint(browserConnection{conn}, rpc.EndpointServices{Binder: binder}, rpc.EndpointOptions{})
		if err != nil {
			_ = conn.CloseNow()
			return
		}
		mu.Lock()
		current = endpoint
		mu.Unlock()
		defer func() {
			_ = endpoint.Close()
			mu.Lock()
			if current == endpoint {
				current = nil
			}
			mu.Unlock()
		}()
		select {
		case <-endpoint.Done():
		case <-ctx.Done():
		}
	})
	mux.HandleFunc("/exercise", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		endpoint := current
		mu.Unlock()
		if endpoint == nil {
			http.Error(w, "peer not connected", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		// Binding checks have no business side effects. Wait for guest publication.
		for {
			client, err := service.BindLaboratoryClient(ctx, endpoint, rpc.BindOptions{})
			if err == nil {
				client.Close()
				break
			}
			if code, _ := rpc.CodeOf(err); code != rpc.CodeUnimplemented {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			select {
			case <-ctx.Done():
				http.Error(w, ctx.Err().Error(), http.StatusGatewayTimeout)
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		report, err := Exercise(ctx, endpoint)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	})
	mux.HandleFunc("/disconnect", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		endpoint := current
		mu.Unlock()
		if endpoint == nil {
			http.Error(w, "peer not connected", http.StatusServiceUnavailable)
			return
		}
		_ = endpoint.Close()
		w.WriteHeader(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	if err := json.NewEncoder(output).Encode(map[string]string{"address": "http://" + listener.Addr().String()}); err != nil {
		return err
	}
	var command string
	_, err = fmt.Fscanln(input, &command)
	return err
}
