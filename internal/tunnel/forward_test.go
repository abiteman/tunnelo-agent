package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// freeAddr reserves an ephemeral port and releases it for the code under
// test to bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestForwarderRelays(t *testing.T) {
	// Target: echoes one line back and closes.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()

	listenAddr := freeAddr(t)
	f := &Forwarder{ListenAddr: listenAddr, TargetAddr: target.Addr().String(), Logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	// Wait for the forwarder to bind.
	var conn net.Conn
	for range 50 {
		conn, err = net.DialTimeout("tcp", listenAddr, time.Second)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dialing forwarder: %v", err)
	}
	defer conn.Close()

	msg := []byte("range-request bytes")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Errorf("relayed %q, want %q", got, msg)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v after cancel", err)
	}
}

func TestForwarderStepsAsideWhenPortServed(t *testing.T) {
	// Jellyfin on bare metal already owns the port the gateway routes to:
	// listen and target ports match, so the relay is redundant.
	existing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	_, port, _ := net.SplitHostPort(existing.Addr().String())

	f := &Forwarder{
		ListenAddr: existing.Addr().String(),
		TargetAddr: "127.0.0.1:" + port,
		Logger:     slog.Default(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := f.Run(ctx); err != nil {
		t.Errorf("Run = %v, want nil (relay not needed)", err)
	}
}

func TestForwarderRefusesPortConflictOnCustomJellyfinPort(t *testing.T) {
	// Jellyfin runs on a custom port, and some unrelated service owns the
	// port the gateway routes to. Stepping aside would publish that
	// unrelated service on the user's subdomain; the forwarder must fail
	// loudly instead.
	unrelated, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer unrelated.Close()

	f := &Forwarder{
		ListenAddr: unrelated.Addr().String(),
		TargetAddr: "127.0.0.1:9096",
		Logger:     slog.Default(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.Run(ctx); err == nil {
		t.Fatal("Run = nil, want error for port conflict with mismatched Jellyfin port")
	}
}

func TestIsUp(t *testing.T) {
	if isUp(time.Time{}) {
		t.Error("isUp(zero) = true, want false before first handshake")
	}
	if !isUp(time.Now().Add(-time.Minute)) {
		t.Error("isUp(1m ago) = false, want true")
	}
	if isUp(time.Now().Add(-10 * time.Minute)) {
		t.Error("isUp(10m ago) = true, want false")
	}
}
