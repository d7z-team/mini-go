package integrations

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/tooling/rpccheck"
)

// Peer executables are built by make test-interop. Each side both serves
// and calls, so the matrix also checks symmetric request and resource ownership.
func TestRPCPeerConformance(t *testing.T) {
	peers := map[string]string{"go": os.Getenv("MINIGO_RPC_GO_PEER"), "rust": os.Getenv("MINIGO_RPC_RUST_PEER")}
	for _, path := range peers {
		if path == "" {
			t.Skip("run make test-interop to build both peers")
		}
	}
	for serverLanguage, serverPath := range peers {
		for clientLanguage, clientPath := range peers {
			t.Run(serverLanguage+"_server_"+clientLanguage+"_client", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				server, serverOutput, _ := startRPCPeer(ctx, t, serverPath, "server", "127.0.0.1:0")
				var ready struct {
					Address string `json:"address"`
				}
				if err := serverOutput.Decode(&ready); err != nil {
					t.Fatal(err)
				}
				if ready.Address == "" {
					t.Fatal("missing peer address")
				}
				client, clientOutput, input := startRPCPeer(ctx, t, clientPath, "client", ready.Address)
				for name, output := range map[string]*json.Decoder{"server": serverOutput, "client": clientOutput} {
					var report rpccheck.Report
					if err := output.Decode(&report); err != nil {
						t.Fatalf("%s report: %v", name, err)
					}
					if report != (rpccheck.Report{Echo: 3, Recursive: true, Resource: 42, Canceled: true}) {
						t.Fatalf("%s: %+v", name, report)
					}
				}
				if _, err := fmt.Fprintln(input, "close"); err != nil {
					t.Fatal(err)
				}
				input.Close()
				if err := client.Wait(); err != nil {
					t.Fatalf("client: %v", err)
				}
				if err := server.Wait(); err != nil {
					t.Fatalf("server: %v", err)
				}
			})
		}
	}
}

func startRPCPeer(ctx context.Context, t *testing.T, path, mode, address string) (*exec.Cmd, *json.Decoder, io.WriteCloser) {
	t.Helper()
	cmd := exec.CommandContext(ctx, path, mode, address)
	cmd.Stderr = os.Stderr
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd, json.NewDecoder(bufio.NewReader(output)), input
}

func TestRPCGatewayConformance(t *testing.T) {
	peers := map[string]string{"go": os.Getenv("MINIGO_RPC_GO_PEER"), "rust": os.Getenv("MINIGO_RPC_RUST_PEER")}
	for _, path := range peers {
		if path == "" {
			t.Skip("run make test-interop to build both peers")
		}
	}
	for serverLanguage, serverPath := range peers {
		for clientLanguage, clientPath := range peers {
			t.Run(serverLanguage+"_gateway_"+clientLanguage+"_publisher", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				server, output, input := startRPCPeer(ctx, t, serverPath, "gateway-server", "127.0.0.1:0")
				var ready struct {
					Address string `json:"address"`
				}
				if err := output.Decode(&ready); err != nil {
					t.Fatal(err)
				}
				if ready.Address == "" {
					t.Fatal("missing Gateway address")
				}
				client, output, _ := startRPCPeer(ctx, t, clientPath, "gateway-client", ready.Address)
				var report rpccheck.GatewayReport
				if err := output.Decode(&report); err != nil {
					t.Fatal(err)
				}
				if report != (rpccheck.GatewayReport{Publications: 3, Resolved: true, Retained: true, Revoked: true}) {
					t.Fatalf("Gateway report: %+v", report)
				}
				if err := client.Wait(); err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintln(input, "close"); err != nil {
					t.Fatal(err)
				}
				input.Close()
				if err := server.Wait(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
