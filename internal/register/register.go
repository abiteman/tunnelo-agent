// Package register exchanges a one-time user token for agent credentials
// and a WireGuard peer configuration, per docs/API.md.
package register

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// JellyfinInfo is the best-effort local Jellyfin detection result included
// in the registration request.
type JellyfinInfo struct {
	Detected   bool   `json:"detected"`
	Version    string `json:"version,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	ServerID   string `json:"server_id,omitempty"`
}

// Request is the body of POST /v1/agents/register.
type Request struct {
	PublicKey    string        `json:"public_key"`
	Hostname     string        `json:"hostname"`
	AgentVersion string        `json:"agent_version"`
	TunnelMode   string        `json:"tunnel_mode"` // TunnelManaged or TunnelExternal
	Jellyfin     *JellyfinInfo `json:"jellyfin"`
}

// Peer describes the gateway-side WireGuard peer.
type Peer struct {
	PublicKey           string   `json:"public_key"`
	Endpoint            string   `json:"endpoint"`
	AllowedIPs          []string `json:"allowed_ips"`
	PersistentKeepalive int      `json:"persistent_keepalive_seconds"`
}

// WireGuardConfig is the tunnel configuration assigned by the gateway.
type WireGuardConfig struct {
	Address string `json:"address"`
	MTU     int    `json:"mtu"`
	Peer    Peer   `json:"peer"`
}

// SpeedtestConfig tells the agent where and how to run the upload test.
type SpeedtestConfig struct {
	UploadURL  string `json:"upload_url"`
	SizeBytes  int64  `json:"size_bytes"`
	MaxSeconds int    `json:"max_seconds"`
}

// Response is the body of a successful registration.
type Response struct {
	AgentID           string          `json:"agent_id"`
	AgentSecret       string          `json:"agent_secret"`
	Subdomain         string          `json:"subdomain"`
	WireGuard         WireGuardConfig `json:"wireguard"`
	ServicePort       int             `json:"service_port"`
	HeartbeatInterval int             `json:"heartbeat_interval_seconds"`
	Speedtest         SpeedtestConfig `json:"speedtest"`
}

// APIError is a structured gateway error response.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway returned %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

// Fatal reports whether the error indicates a bad credential, where retrying
// is pointless and the agent should stop with an actionable message.
func (e *APIError) Fatal() bool {
	return e.Code == "invalid_token" || e.Code == "invalid_agent"
}

// Retryable reports whether the gateway might accept the same request later.
// Per the contract, 5xx (including 503 not_ready) may be retried with
// backoff; 4xx means the request itself is wrong and repeating it can't
// help — except 429, which is a pacing signal rather than a rejection.
func (e *APIError) Retryable() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// ParseErrorBody decodes the gateway's JSON error envelope from a non-2xx
// response. It always returns a non-nil *APIError, falling back to the raw
// body when the envelope doesn't parse.
func ParseErrorBody(status int, body []byte) *APIError {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		return &APIError{StatusCode: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	return &APIError{StatusCode: status, Code: "unknown", Message: strings.TrimSpace(string(body))}
}

// Client talks to the gateway registration endpoint.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient returns a Client for the given gateway base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Register exchanges the user token for agent credentials and a WireGuard
// configuration.
func (c *Client) Register(ctx context.Context, token string, reg Request) (*Response, error) {
	payload, err := json.Marshal(reg)
	if err != nil {
		return nil, fmt.Errorf("encoding registration request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/agents/register", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling gateway: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading gateway response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ParseErrorBody(resp.StatusCode, body)
	}

	var out Response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding registration response: %w", err)
	}
	if err := validate(&out); err != nil {
		return nil, fmt.Errorf("invalid registration response: %w", err)
	}
	return &out, nil
}

func validate(r *Response) error {
	switch {
	case r.AgentID == "":
		return fmt.Errorf("missing agent_id")
	case r.AgentSecret == "":
		return fmt.Errorf("missing agent_secret")
	case r.WireGuard.Address == "":
		return fmt.Errorf("missing wireguard.address")
	case r.WireGuard.Peer.PublicKey == "":
		return fmt.Errorf("missing wireguard.peer.public_key")
	case r.WireGuard.Peer.Endpoint == "":
		return fmt.Errorf("missing wireguard.peer.endpoint")
	case len(r.WireGuard.Peer.AllowedIPs) == 0:
		return fmt.Errorf("missing wireguard.peer.allowed_ips")
	}
	if r.HeartbeatInterval <= 0 {
		r.HeartbeatInterval = 60
	}
	if r.WireGuard.MTU <= 0 {
		r.WireGuard.MTU = 1280
	}
	return nil
}
