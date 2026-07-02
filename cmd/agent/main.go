// Command agent is the Tunnelo client: it registers this host with the
// gateway, keeps a WireGuard tunnel up, and reports health so the user's
// subdomain always reflects reality.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/abiteman/tunnelo-agent/internal/config"
	"github.com/abiteman/tunnelo-agent/internal/detect"
	"github.com/abiteman/tunnelo-agent/internal/heartbeat"
	"github.com/abiteman/tunnelo-agent/internal/register"
	"github.com/abiteman/tunnelo-agent/internal/speedtest"
	"github.com/abiteman/tunnelo-agent/internal/tunnel"
)

// version is injected by goreleaser at build time.
var version = "dev"

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent exiting", "error", err)
		os.Exit(1)
	}
	log.Info("agent stopped")
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("tunnelo agent starting", "version", version, "gateway", cfg.GatewayURL)

	jellyfin := detect.New(cfg.JellyfinURL)

	state, err := register.LoadState(cfg.StateDir)
	if err != nil {
		return err
	}
	if state == nil {
		if state, err = registerAgent(ctx, cfg, jellyfin, log); err != nil {
			return err
		}
	} else {
		log.Info("using existing registration", "agent_id", state.AgentID, "subdomain", state.Subdomain)
	}

	group, ctx := errgroup.WithContext(ctx)

	// In external mode the user's own WireGuard carries the tunnel; the
	// agent only renders the peer config and keeps reporting health.
	var tunnelSource heartbeat.TunnelStatusSource
	if cfg.TunnelMode == register.TunnelExternal {
		if err := writeWgQuickConfig(cfg, state, log); err != nil {
			return err
		}
	} else {
		tunnelIP, manager, err := buildTunnel(cfg, state, log)
		if err != nil {
			return err
		}
		tunnelSource = manager
		group.Go(func() error { return manager.Run(ctx) })

		jellyfinAddr, err := cfg.JellyfinHostPort()
		if err != nil {
			return err
		}
		forwarder := &tunnel.Forwarder{
			ListenAddr: fmt.Sprintf("%s:%d", tunnelIP, servicePort(state)),
			TargetAddr: jellyfinAddr,
			Logger:     log,
		}
		group.Go(func() error { return forwarder.Run(ctx) })
	}

	sender := &heartbeat.Sender{
		GatewayURL:   cfg.GatewayURL,
		AgentID:      state.AgentID,
		AgentSecret:  state.AgentSecret,
		AgentVersion: version,
		Interval:     time.Duration(state.HeartbeatInterval) * time.Second,
		Tunnel:       tunnelSource,
		Jellyfin:     jellyfin,
		Logger:       log,
	}
	group.Go(func() error {
		err := sender.Run(ctx)
		if errors.Is(err, heartbeat.ErrAgentRevoked) {
			// Key regenerated from the dashboard: this registration is dead.
			log.Error("gateway revoked this agent; removing local state — restart with a fresh TUNNELO_TOKEN to re-register")
			if rmErr := register.Remove(cfg.StateDir); rmErr != nil {
				log.Error("removing state", "error", rmErr)
			}
		}
		return err
	})

	if cfg.Speedtest || !state.SpeedtestDone {
		group.Go(func() error {
			runSpeedtest(ctx, cfg, state, log)
			return nil
		})
	}

	log.Info("agent running", "subdomain", state.Subdomain)
	return group.Wait()
}

// registerAgent generates a keypair and exchanges the user token for a
// tunnel configuration, retrying transient gateway errors with backoff.
func registerAgent(ctx context.Context, cfg *config.Config, jellyfin *detect.Prober, log *slog.Logger) (*register.State, error) {
	if cfg.Token == "" {
		return nil, errors.New("not registered yet: set TUNNELO_TOKEN (or --token) to the token from your Tunnelo dashboard")
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generating WireGuard keypair: %w", err)
	}

	req := register.Request{
		PublicKey:    key.PublicKey().String(),
		AgentVersion: version,
		TunnelMode:   cfg.TunnelMode,
		Jellyfin:     detectJellyfin(ctx, jellyfin, log),
	}
	if req.Hostname, err = os.Hostname(); err != nil {
		req.Hostname = "unknown"
	}

	client := register.NewClient(cfg.GatewayURL)
	backoff := 2 * time.Second
	var resp *register.Response
	for {
		resp, err = client.Register(ctx, cfg.Token, req)
		if err == nil {
			break
		}
		var apiErr *register.APIError
		if errors.As(err, &apiErr) {
			if apiErr.Fatal() {
				return nil, fmt.Errorf("registration rejected — check your token in the Tunnelo dashboard: %w", err)
			}
			if !apiErr.Retryable() {
				return nil, fmt.Errorf("gateway rejected the registration request (agent/gateway version mismatch?): %w", err)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn("registration failed, retrying", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}

	state := &register.State{
		AgentID:           resp.AgentID,
		AgentSecret:       resp.AgentSecret,
		Subdomain:         resp.Subdomain,
		PrivateKey:        key.String(),
		WireGuard:         resp.WireGuard,
		ServicePort:       resp.ServicePort,
		HeartbeatInterval: resp.HeartbeatInterval,
		Speedtest:         resp.Speedtest,
	}
	if err := state.Save(cfg.StateDir); err != nil {
		return nil, err
	}
	log.Info("registered with gateway", "agent_id", state.AgentID, "subdomain", state.Subdomain)
	return state, nil
}

func detectJellyfin(ctx context.Context, prober *detect.Prober, log *slog.Logger) *register.JellyfinInfo {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := prober.Probe(probeCtx)
	if err != nil {
		log.Warn("jellyfin not detected (will keep checking via heartbeat)", "url", prober.BaseURL, "error", err)
		return &register.JellyfinInfo{Detected: false}
	}
	log.Info("jellyfin detected", "version", info.Version, "server_name", info.ServerName)
	return &register.JellyfinInfo{
		Detected:   true,
		Version:    info.Version,
		ServerName: info.ServerName,
		ServerID:   info.ID,
	}
}

func buildTunnel(cfg *config.Config, state *register.State, log *slog.Logger) (string, *tunnel.Manager, error) {
	tcfg := tunnel.Config{
		InterfaceName:  cfg.Interface,
		PrivateKey:     state.PrivateKey,
		Address:        state.WireGuard.Address,
		MTU:            state.WireGuard.MTU,
		ForceUserspace: cfg.Userspace,
		Peer: tunnel.PeerConfig{
			PublicKey:           state.WireGuard.Peer.PublicKey,
			Endpoint:            state.WireGuard.Peer.Endpoint,
			AllowedIPs:          state.WireGuard.Peer.AllowedIPs,
			PersistentKeepalive: time.Duration(state.WireGuard.Peer.PersistentKeepalive) * time.Second,
		},
	}
	ip, err := tcfg.TunnelIP()
	if err != nil {
		return "", nil, err
	}
	return ip, tunnel.NewManager(tcfg, log), nil
}

// writeWgQuickConfig renders the peer config for the user's own WireGuard.
// An existing file is left untouched (the user may have merged or edited
// it); delete it to regenerate.
func writeWgQuickConfig(cfg *config.Config, state *register.State, log *slog.Logger) error {
	path := cfg.WgConfigOut
	if path == "" {
		path = filepath.Join(cfg.StateDir, "tunnelo-wg.conf")
	}
	if _, err := os.Stat(path); err == nil {
		log.Info("external tunnel mode: wg-quick config already written", "path", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(state.WgQuickConfig()), 0o600); err != nil {
		return fmt.Errorf("writing wg-quick config: %w", err)
	}
	log.Info("external tunnel mode: wrote wg-quick config — add it to your WireGuard setup",
		"path", path,
		"note", fmt.Sprintf("the gateway routes %s to your tunnel IP on port %d", state.Subdomain, servicePort(state)))
	return nil
}

func servicePort(state *register.State) int {
	if state.ServicePort > 0 {
		return state.ServicePort
	}
	return 8096
}

// runSpeedtest is best-effort: a failed test never takes the agent down.
func runSpeedtest(ctx context.Context, cfg *config.Config, state *register.State, log *slog.Logger) {
	runner := &speedtest.Runner{
		GatewayURL:  cfg.GatewayURL,
		AgentID:     state.AgentID,
		AgentSecret: state.AgentSecret,
		Config:      state.Speedtest,
	}
	result, err := runner.Run(ctx)
	if result != nil {
		log.Info("upload speed test complete — this is your ISP's ceiling for remote streaming",
			"upload_mbps", fmt.Sprintf("%.1f", result.UploadMbps),
			"bytes_sent", result.BytesSent)
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("speed test", "error", err)
		}
		return
	}
	state.SpeedtestDone = true
	if err := state.Save(cfg.StateDir); err != nil {
		log.Warn("saving state after speed test", "error", err)
	}
}
