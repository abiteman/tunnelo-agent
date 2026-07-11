// Package heartbeat periodically reports tunnel and Jellyfin health to the
// gateway, which uses it to show connection status and serve a friendly
// "server offline" page instead of proxy timeouts.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/abiteman/tunnelo-agent/internal/detect"
	"github.com/abiteman/tunnelo-agent/internal/register"
	"github.com/abiteman/tunnelo-agent/internal/tunnel"
)

// TunnelStatus mirrors the API contract's heartbeat tunnel object.
type TunnelStatus struct {
	Up                bool  `json:"up"`
	LastHandshakeUnix int64 `json:"last_handshake_unix,omitempty"`
	RxBytes           int64 `json:"rx_bytes"`
	TxBytes           int64 `json:"tx_bytes"`
}

// JellyfinStatus mirrors the API contract's heartbeat jellyfin object.
// Kept for gateways that predate the service-agnostic contract; it mirrors
// ServiceStatus (version only when the service really is Jellyfin).
type JellyfinStatus struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
}

// ServiceStatus reports the exposed service's health, whatever it is.
type ServiceStatus struct {
	Reachable bool   `json:"reachable"`
	Type      string `json:"type,omitempty"`
	Version   string `json:"version,omitempty"`
}

// Report is the heartbeat request body. Tunnel is omitted in external
// tunnel mode: the gateway terminates the tunnel and reads handshake state
// from its own peer table.
type Report struct {
	AgentVersion string         `json:"agent_version"`
	Tunnel       *TunnelStatus  `json:"tunnel,omitempty"`
	Service      ServiceStatus  `json:"service"`
	Jellyfin     JellyfinStatus `json:"jellyfin"`
}

// TunnelStatusSource provides tunnel state for heartbeats. A nil source
// means the tunnel is managed outside the agent.
type TunnelStatusSource interface {
	Status() tunnel.Status
}

// ErrAgentRevoked is returned by Run when the gateway rejects the agent
// credentials; the caller should discard persisted state and require
// re-registration.
var ErrAgentRevoked = errors.New("agent credentials revoked by gateway")

// Sender posts heartbeats on the gateway-provided interval.
type Sender struct {
	GatewayURL   string
	AgentID      string
	AgentSecret  string
	AgentVersion string
	Interval     time.Duration
	Tunnel       TunnelStatusSource // nil when the user brings their own WireGuard
	Service      *detect.Prober
	Logger       *slog.Logger
	HTTPClient   *http.Client
}

// Run sends heartbeats until ctx is cancelled. Transient failures are logged
// and retried on the next tick; a credential rejection stops the loop with
// ErrAgentRevoked.
func (s *Sender) Run(ctx context.Context) error {
	if s.Interval <= 0 {
		s.Interval = time.Minute
	}
	log := s.Logger.With("component", "heartbeat")

	timer := time.NewTimer(s.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}

		if err := s.beat(ctx); err != nil {
			var apiErr *register.APIError
			if errors.As(err, &apiErr) && apiErr.Fatal() {
				return fmt.Errorf("%w: %v", ErrAgentRevoked, err)
			}
			if ctx.Err() == nil {
				log.Warn("heartbeat failed", "error", err)
			}
		}
		timer.Reset(s.Interval)
	}
}

// beat gathers status and posts one heartbeat. The gateway may adjust the
// interval in its response.
func (s *Sender) beat(ctx context.Context) error {
	svc, jf := s.serviceStatus(ctx)
	report := Report{AgentVersion: s.AgentVersion, Tunnel: s.tunnelStatus(), Service: svc, Jellyfin: jf}

	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	url := strings.TrimRight(s.GatewayURL, "/") + "/v1/agents/" + s.AgentID + "/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.AgentSecret)

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return register.ParseErrorBody(resp.StatusCode, body)
	}

	var out struct {
		HeartbeatInterval int `json:"heartbeat_interval_seconds"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.HeartbeatInterval > 0 {
		s.Interval = time.Duration(out.HeartbeatInterval) * time.Second
	}
	return nil
}

func (s *Sender) tunnelStatus() *TunnelStatus {
	if s.Tunnel == nil {
		return nil
	}
	st := s.Tunnel.Status()
	ts := &TunnelStatus{Up: st.Up, RxBytes: st.RxBytes, TxBytes: st.TxBytes}
	if !st.LastHandshake.IsZero() {
		ts.LastHandshakeUnix = st.LastHandshake.Unix()
	}
	return ts
}

// serviceStatus probes the exposed service and renders both the modern
// service block and the legacy jellyfin mirror (version only when the
// service really is Jellyfin, so old dashboards never show a Navidrome
// version as a Jellyfin one).
func (s *Sender) serviceStatus(ctx context.Context) (ServiceStatus, JellyfinStatus) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := s.Service.Probe(probeCtx)
	svc := ServiceStatus{Reachable: res.Reachable, Type: res.Type, Version: res.Version}
	jf := JellyfinStatus{Reachable: res.Reachable}
	if res.Type == "jellyfin" {
		jf.Version = res.Version
	}
	return svc, jf
}
