// Package tunnel maintains the WireGuard tunnel to the gateway: a wg-quick
// equivalent built on wgctrl, with a userspace wireguard-go fallback for
// hosts without the kernel module, supervised by a persistent reconnect
// loop.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// handshakeTimeout is how stale the last handshake may be before the tunnel
// is considered down. WireGuard re-handshakes at least every ~2 minutes with
// persistent keepalive on, so 3 minutes of silence means trouble.
const handshakeTimeout = 3 * time.Minute

const (
	checkInterval   = 15 * time.Second
	refreshInterval = 30 * time.Second
	maxBackoff      = 60 * time.Second
)

// PeerConfig describes the gateway-side peer.
type PeerConfig struct {
	PublicKey           string
	Endpoint            string // host:port; re-resolved on reconnect
	AllowedIPs          []string
	PersistentKeepalive time.Duration
}

// Config is everything needed to bring the tunnel up.
type Config struct {
	InterfaceName  string
	PrivateKey     string // base64
	Address        string // CIDR, e.g. 10.66.0.7/32
	MTU            int
	Peer           PeerConfig
	ForceUserspace bool // skip the kernel module even if available
}

// Status is a point-in-time view of the tunnel, safe to read from other
// goroutines (used by heartbeat).
type Status struct {
	Up            bool
	Mode          string // "kernel" or "userspace"
	LastHandshake time.Time
	RxBytes       int64
	TxBytes       int64
}

// TunnelIP returns the local tunnel address without its prefix length.
func (c Config) TunnelIP() (string, error) {
	ip, _, err := net.ParseCIDR(c.Address)
	if err != nil {
		return "", fmt.Errorf("parsing tunnel address %q: %w", c.Address, err)
	}
	return ip.String(), nil
}

// wgDevice is the platform-specific half, implemented in tunnel_linux.go
// (and stubbed elsewhere).
type wgDevice interface {
	// RefreshPeer re-resolves the endpoint and reapplies the peer config,
	// prodding WireGuard into a fresh handshake attempt.
	RefreshPeer() error
	Stats() (Status, error)
	Close() error
}

// Manager owns the tunnel lifecycle.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu     sync.RWMutex
	status Status
}

// NewManager returns a Manager for cfg.
func NewManager(cfg Config, log *slog.Logger) *Manager {
	if cfg.InterfaceName == "" {
		cfg.InterfaceName = "tunnelo0"
	}
	return &Manager{cfg: cfg, log: log.With("component", "tunnel")}
}

// Status returns the last observed tunnel state.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) setStatus(s Status) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

// Run brings the tunnel up and keeps it up until ctx is cancelled, tearing
// the interface down on exit. Device creation failures are retried with
// exponential backoff; a device that stops responding is recreated.
func (m *Manager) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		dev, mode, err := openDevice(m.cfg, m.log)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.log.Error("bringing tunnel up failed", "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		m.log.Info("tunnel up", "interface", m.cfg.InterfaceName, "mode", mode,
			"endpoint", m.cfg.Peer.Endpoint)

		m.supervise(ctx, dev, mode)

		if err := dev.Close(); err != nil {
			m.log.Warn("closing tunnel device", "error", err)
		}
		m.setStatus(Status{Mode: mode})
		if ctx.Err() != nil {
			m.log.Info("tunnel down", "interface", m.cfg.InterfaceName)
			return ctx.Err()
		}
		m.log.Warn("tunnel device lost, recreating", "interface", m.cfg.InterfaceName)
	}
}

// supervise polls the device until ctx is cancelled or the device stops
// answering (in which case it returns so Run can recreate it). While the
// handshake is stale it periodically refreshes the peer, which re-resolves
// the gateway endpoint — this is what recovers from gateway IP changes and
// long network outages.
func (m *Manager) supervise(ctx context.Context, dev wgDevice, mode string) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	up := time.Now()
	var lastRefresh time.Time
	wasUp := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		st, err := dev.Stats()
		if err != nil {
			m.log.Error("reading tunnel stats", "error", err)
			return
		}
		st.Mode = mode
		st.Up = isUp(st.LastHandshake)
		m.setStatus(st)

		if st.Up != wasUp {
			if st.Up {
				m.log.Info("tunnel connected", "last_handshake", st.LastHandshake)
			} else {
				m.log.Warn("tunnel lost handshake", "last_handshake", st.LastHandshake)
			}
			wasUp = st.Up
		}

		stale := !st.Up && time.Since(up) > handshakeTimeout
		if stale && time.Since(lastRefresh) >= refreshInterval {
			lastRefresh = time.Now()
			if err := dev.RefreshPeer(); err != nil {
				m.log.Error("refreshing peer", "error", err)
			} else {
				m.log.Info("refreshed peer endpoint", "endpoint", m.cfg.Peer.Endpoint)
			}
		}
	}
}

// isUp reports whether a handshake happened recently enough to consider the
// tunnel live.
func isUp(lastHandshake time.Time) bool {
	return !lastHandshake.IsZero() && time.Since(lastHandshake) < handshakeTimeout
}
