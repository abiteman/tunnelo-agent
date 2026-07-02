package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"
)

// Forwarder relays TCP connections arriving on the tunnel address (the
// gateway proxies user traffic to tunnel-IP:service-port) to the local
// Jellyfin server. This is what makes the Docker deployment work, where
// Jellyfin runs in a different container and nothing on the agent's network
// namespace would otherwise answer on the tunnel IP.
//
// On bare metal, Jellyfin typically listens on 0.0.0.0 and answers tunnel
// traffic directly; the bind then fails with "address in use" and the
// forwarder steps aside.
type Forwarder struct {
	ListenAddr string // tunnel-IP:port
	TargetAddr string // Jellyfin host:port
	Logger     *slog.Logger
}

// Run binds ListenAddr and relays connections to TargetAddr until ctx is
// cancelled. The bind is retried while the tunnel interface is still coming
// up; an "address in use" result is treated as "a local listener already
// serves this port" and turns Run into a no-op.
func (f *Forwarder) Run(ctx context.Context) error {
	log := f.Logger.With("component", "forward", "listen", f.ListenAddr, "target", f.TargetAddr)

	ln, err := f.bind(ctx)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			log.Info("port already served locally; relay not needed")
			<-ctx.Done()
			return nil
		}
		return err
	}
	defer ln.Close()
	log.Info("relaying gateway traffic to local service")

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting connection: %w", err)
		}
		go f.relay(ctx, conn, log)
	}
}

// bind retries until the listen succeeds, the address is genuinely in use,
// or ctx ends. Retrying covers the window where the tunnel interface (and
// therefore the tunnel IP) doesn't exist yet.
func (f *Forwarder) bind(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	for {
		ln, err := lc.Listen(ctx, "tcp", f.ListenAddr)
		if err == nil || errors.Is(err, syscall.EADDRINUSE) {
			return ln, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (f *Forwarder) relay(ctx context.Context, src net.Conn, log *slog.Logger) {
	defer src.Close()

	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	dst, err := d.DialContext(dialCtx, "tcp", f.TargetAddr)
	cancel()
	if err != nil {
		log.Warn("local service unreachable", "error", err)
		return
	}
	defer dst.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, dst, src)
	go pipe(&wg, src, dst)
	wg.Wait()
}

// pipe copies one direction and half-closes the destination so the peer
// sees EOF, which streaming clients rely on.
func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	io.Copy(dst, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	} else {
		dst.Close()
	}
}
