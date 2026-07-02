package register

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func validResponse() map[string]any {
	return map[string]any{
		"agent_id":     "agt_test",
		"agent_secret": "as_secret",
		"subdomain":    "brave-otter.tunnelo.io",
		"wireguard": map[string]any{
			"address": "10.66.0.7/32",
			"mtu":     1280,
			"peer": map[string]any{
				"public_key":                   "gwpubkey=",
				"endpoint":                     "gw1.tunnelo.io:51820",
				"allowed_ips":                  []string{"10.66.0.1/32"},
				"persistent_keepalive_seconds": 25,
			},
		},
		"service_port":               8096,
		"heartbeat_interval_seconds": 60,
		"speedtest": map[string]any{
			"upload_url":  "https://gw1.tunnelo.io/v1/speedtest/sink",
			"size_bytes":  33554432,
			"max_seconds": 15,
		},
	}
}

func TestRegisterSuccess(t *testing.T) {
	var gotAuth string
	var gotReq Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/register" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		json.NewEncoder(w).Encode(validResponse())
	}))
	defer srv.Close()

	resp, err := NewClient(srv.URL).Register(context.Background(), "usertoken", Request{
		PublicKey:    "agentpubkey=",
		Hostname:     "media-box",
		AgentVersion: "v0.1.0",
		Jellyfin:     &JellyfinInfo{Detected: true, Version: "10.10.3"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if gotAuth != "Bearer usertoken" {
		t.Errorf("Authorization = %q, want Bearer usertoken", gotAuth)
	}
	if gotReq.PublicKey != "agentpubkey=" {
		t.Errorf("request public_key = %q", gotReq.PublicKey)
	}
	if gotReq.Jellyfin == nil || !gotReq.Jellyfin.Detected {
		t.Errorf("request jellyfin = %+v, want detected", gotReq.Jellyfin)
	}
	if resp.AgentID != "agt_test" || resp.AgentSecret != "as_secret" {
		t.Errorf("credentials = %q/%q", resp.AgentID, resp.AgentSecret)
	}
	if resp.WireGuard.Peer.Endpoint != "gw1.tunnelo.io:51820" {
		t.Errorf("peer endpoint = %q", resp.WireGuard.Peer.Endpoint)
	}
	if resp.HeartbeatInterval != 60 {
		t.Errorf("heartbeat interval = %d", resp.HeartbeatInterval)
	}
}

func TestRegisterDefaultsApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := validResponse()
		delete(body, "heartbeat_interval_seconds")
		wg := body["wireguard"].(map[string]any)
		delete(wg, "mtu")
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	resp, err := NewClient(srv.URL).Register(context.Background(), "t", Request{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.HeartbeatInterval != 60 {
		t.Errorf("default heartbeat interval = %d, want 60", resp.HeartbeatInterval)
	}
	if resp.WireGuard.MTU != 1280 {
		t.Errorf("default MTU = %d, want 1280", resp.WireGuard.MTU)
	}
}

func TestRegisterInvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"invalid_token","message":"token revoked"}}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Register(context.Background(), "bad", Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.Code != "invalid_token" || !apiErr.Fatal() {
		t.Errorf("apiErr = %+v, want fatal invalid_token", apiErr)
	}
}

func TestRegisterMalformedErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).Register(context.Background(), "t", Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Fatal() {
		t.Errorf("apiErr = %+v, want retryable 502", apiErr)
	}
}

func TestRegisterIncompleteResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := validResponse()
		delete(body, "wireguard")
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL).Register(context.Background(), "t", Request{}); err == nil {
		t.Fatal("Register succeeded on response missing wireguard config")
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if s, err := LoadState(dir); err != nil || s != nil {
		t.Fatalf("LoadState on empty dir = %v, %v; want nil, nil", s, err)
	}

	want := &State{
		AgentID:           "agt_test",
		AgentSecret:       "as_secret",
		PrivateKey:        "privkey=",
		WireGuard:         WireGuardConfig{Address: "10.66.0.7/32", MTU: 1280},
		ServicePort:       8096,
		HeartbeatInterval: 60,
	}
	if err := want.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.AgentID != want.AgentID || got.PrivateKey != want.PrivateKey || got.WireGuard.Address != want.WireGuard.Address {
		t.Errorf("LoadState = %+v, want %+v", got, want)
	}

	if err := Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s, err := LoadState(dir); err != nil || s != nil {
		t.Fatalf("LoadState after Remove = %v, %v; want nil, nil", s, err)
	}
}

func TestStatePermissions(t *testing.T) {
	dir := t.TempDir()
	s := &State{AgentID: "a"}
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state.json mode = %o, want 600", perm)
	}
}
