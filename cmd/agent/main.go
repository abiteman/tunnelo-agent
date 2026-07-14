// Command agent is the Tunnelo client: it registers this host with the
// gateway, keeps a WireGuard tunnel up, and reports health so the user's
// subdomain always reflects reality.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
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

// runService is one resolved service at runtime: the local spec joined with
// the gateway's subdomain/name assignment, plus its prober.
type runService struct {
	spec   config.ServiceSpec
	name   string
	sub    string
	prober *detect.Prober
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("tunnelo agent starting", "version", version, "gateway", cfg.GatewayURL, "services", len(cfg.Services))

	state, err := register.LoadState(cfg.StateDir)
	if err != nil {
		return err
	}
	switch {
	case state == nil:
		if state, err = registerAgent(ctx, cfg, nil, log); err != nil {
			return err
		}
	case !samePorts(state.Ports(), specPorts(cfg.Services)) && cfg.Token == "":
		// The service set changed but there's no token to re-register with
		// (tokens are one-time setup credentials). Keep running with the
		// existing services rather than failing; the user sets TUNNELO_TOKEN
		// to apply the change.
		log.Warn("service set changed but TUNNELO_TOKEN is not set; keeping the existing services — set the token to apply the change",
			"registered", state.Ports(), "configured", specPorts(cfg.Services))
	case !samePorts(state.Ports(), specPorts(cfg.Services)):
		// The configured service set changed: re-register (reusing the existing
		// key so the tunnel doesn't reset) to pick up added/removed services.
		log.Info("service set changed since last run; re-registering",
			"was", state.Ports(), "now", specPorts(cfg.Services))
		if state, err = registerAgent(ctx, cfg, state, log); err != nil {
			return err
		}
	default:
		log.Info("using existing registration", "agent_id", state.AgentID,
			"subdomain", state.Subdomain, "services", len(state.Services))
	}

	services, err := buildServices(cfg, state)
	if err != nil {
		return err
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

		// One forwarder per service: the tunnel-side listen port equals the
		// service's local port, and each dials its own local target.
		for _, sv := range services {
			port := strconv.Itoa(sv.spec.Port)
			forwarder := &tunnel.Forwarder{
				// net.JoinHostPort brackets IPv6 hosts (e.g. [::1]:8096) so
				// net.Dial/Listen accept them.
				ListenAddr: net.JoinHostPort(tunnelIP, port),
				TargetAddr: net.JoinHostPort(sv.spec.Host, port),
				Logger:     log.With("service", sv.name, "subdomain", sv.sub),
			}
			group.Go(func() error { return forwarder.Run(ctx) })
		}
	}

	// Dashboard-requested speed test re-runs arrive via heartbeat
	// responses; single-flight so a duplicate request can't stack tests.
	var speedtestRunning atomic.Bool
	hbServices := make([]heartbeat.Service, 0, len(services))
	for _, sv := range services {
		hbServices = append(hbServices, heartbeat.Service{Name: sv.name, Prober: sv.prober})
	}
	sender := &heartbeat.Sender{
		GatewayURL:   cfg.GatewayURL,
		AgentID:      state.AgentID,
		AgentSecret:  state.AgentSecret,
		AgentVersion: version,
		Interval:     time.Duration(state.HeartbeatInterval) * time.Second,
		Tunnel:       tunnelSource,
		Services:     hbServices,
		OnSpeedtestRequest: func() {
			if !speedtestRunning.CompareAndSwap(false, true) {
				return
			}
			log.Info("speed test re-run requested from dashboard")
			go func() {
				defer speedtestRunning.Store(false)
				runSpeedtest(ctx, cfg, state, log)
			}()
		},
		Logger: log,
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

	log.Info("agent running", "subdomain", state.Subdomain, "services", len(services))
	return group.Wait()
}

// buildServices joins the gateway's per-service assignments (state) with the
// local specs (config) by port, building each service's prober.
func buildServices(cfg *config.Config, state *register.State) ([]runService, error) {
	byPort := make(map[int]config.ServiceSpec, len(cfg.Services))
	for _, sp := range cfg.Services {
		byPort[sp.Port] = sp
	}
	out := make([]runService, 0, len(state.Services))
	for _, ss := range state.Services {
		sp, ok := byPort[ss.Port]
		if !ok {
			// A port-set change triggers re-registration, so state and config
			// normally agree; skip anything stale defensively.
			continue
		}
		out = append(out, runService{
			spec:   sp,
			name:   ss.Name,
			sub:    ss.Subdomain,
			prober: detect.New(sp.URL, sp.HealthPath, sp.Type),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no services resolved from the registration")
	}
	return out, nil
}

// registerAgent exchanges the user token for a tunnel configuration, retrying
// transient gateway errors with backoff. When prev is non-nil the existing
// WireGuard key is reused (a re-registration for a changed service set must
// not reset the tunnel).
func registerAgent(ctx context.Context, cfg *config.Config, prev *register.State, log *slog.Logger) (*register.State, error) {
	if cfg.Token == "" {
		return nil, errors.New("not registered yet: set TUNNELO_TOKEN (or --token) to the token from your Tunnelo dashboard")
	}

	key, err := registrationKey(prev)
	if err != nil {
		return nil, err
	}

	reports := make([]register.ServiceReport, 0, len(cfg.Services))
	for _, sp := range cfg.Services {
		res := detectOne(ctx, sp, log)
		reports = append(reports, register.ServiceReport{
			Port: sp.Port, Type: res.Type, Detected: res.Reachable, Version: res.Version,
		})
	}
	req := register.Request{
		PublicKey:    key.PublicKey().String(),
		AgentVersion: version,
		TunnelMode:   cfg.TunnelMode,
		Services:     reports,
	}
	if req.Hostname, err = os.Hostname(); err != nil {
		req.Hostname = "unknown"
	}

	resp, err := registerWithRetry(ctx, cfg, req, log)
	if err != nil {
		return nil, err
	}

	state := &register.State{
		AgentID:           resp.AgentID,
		AgentSecret:       resp.AgentSecret,
		Subdomain:         resp.Subdomain,
		PrivateKey:        key.String(),
		WireGuard:         resp.WireGuard,
		ServicePort:       resp.ServicePort,
		Services:          serviceStates(resp),
		HeartbeatInterval: resp.HeartbeatInterval,
		Speedtest:         resp.Speedtest,
	}
	if err := state.Save(cfg.StateDir); err != nil {
		return nil, err
	}
	log.Info("registered with gateway", "agent_id", state.AgentID,
		"subdomain", state.Subdomain, "services", len(state.Services))
	return state, nil
}

// registrationKey reuses prev's key (no tunnel reset on re-register) or
// generates a fresh one.
func registrationKey(prev *register.State) (wgtypes.Key, error) {
	if prev != nil && prev.PrivateKey != "" {
		key, err := wgtypes.ParseKey(prev.PrivateKey)
		if err != nil {
			return wgtypes.Key{}, fmt.Errorf("parsing existing WireGuard key: %w", err)
		}
		return key, nil
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("generating WireGuard keypair: %w", err)
	}
	return key, nil
}

// registerWithRetry posts the registration, retrying transient errors.
func registerWithRetry(ctx context.Context, cfg *config.Config, req register.Request, log *slog.Logger) (*register.Response, error) {
	client := register.NewClient(cfg.GatewayURL)
	backoff := 2 * time.Second
	for {
		resp, err := client.Register(ctx, cfg.Token, req)
		if err == nil {
			return resp, nil
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
}

// serviceStates maps the registration response's service assignments to
// persisted state, synthesizing a single primary from the top-level fields if
// the gateway returned no services array.
func serviceStates(resp *register.Response) []register.ServiceState {
	if len(resp.Services) == 0 {
		return []register.ServiceState{{Name: resp.Subdomain, Subdomain: resp.Subdomain, Port: resp.ServicePort}}
	}
	out := make([]register.ServiceState, 0, len(resp.Services))
	for _, s := range resp.Services {
		out = append(out, register.ServiceState{Name: s.Name, Subdomain: s.Subdomain, Port: s.ServicePort})
	}
	return out
}

// detectOne probes one service for the registration request.
func detectOne(ctx context.Context, sp config.ServiceSpec, log *slog.Logger) detect.Result {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := detect.New(sp.URL, sp.HealthPath, sp.Type).Probe(probeCtx)
	if res.Reachable {
		log.Info("service detected", "url", sp.URL, "type", res.Type, "version", res.Version)
	} else {
		log.Warn("service not detected (will keep checking via heartbeat)", "url", sp.URL, "error", res.Err)
	}
	return res
}

// samePorts reports whether two port sets are equal regardless of order.
func samePorts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int]int, len(a))
	for _, p := range a {
		seen[p]++
	}
	for _, p := range b {
		seen[p]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// specPorts returns the local ports of the configured services.
func specPorts(specs []config.ServiceSpec) []int {
	ports := make([]int, 0, len(specs))
	for _, sp := range specs {
		ports = append(ports, sp.Port)
	}
	return ports
}

func buildTunnel(cfg *config.Config, state *register.State, log *slog.Logger) (string, *tunnel.Manager, error) {
	// MTU: explicit override > registration value > conservative fallback.
	// 1280 (IPv6 minimum) survives the CGNAT/464XLAT/PPPoE paths common on
	// residential links, where WireGuard's usual 1420 silently black-holes
	// full-size packets: handshakes pass, TCP data stalls.
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = state.WireGuard.MTU
	}
	if mtu == 0 {
		mtu = 1280
	}
	tcfg := tunnel.Config{
		InterfaceName:  cfg.Interface,
		PrivateKey:     state.PrivateKey,
		Address:        state.WireGuard.Address,
		MTU:            mtu,
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
