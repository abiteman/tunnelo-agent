package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abiteman/tunnelo-agent/internal/detect"
	"github.com/abiteman/tunnelo-agent/internal/tunnel"
)

func newSender(t *testing.T, gatewayURL, jellyfinURL string) *Sender {
	t.Helper()
	return &Sender{
		GatewayURL:   gatewayURL,
		AgentID:      "agt_1",
		AgentSecret:  "as_secret",
		AgentVersion: "v0.1.0-test",
		Interval:     10 * time.Millisecond,
		Tunnel:       tunnel.NewManager(tunnel.Config{}, slog.Default()),
		Service:      detect.New(jellyfinURL, "", ""),
		Logger:       slog.Default(),
	}
}

func TestHeartbeatReportsStatus(t *testing.T) {
	jellyfin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"10.10.3","Id":"abc"}`))
	}))
	defer jellyfin.Close()

	reports := make(chan Report, 1)
	var gotAuth string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/agt_1/heartbeat" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var rep Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			t.Errorf("decoding report: %v", err)
		}
		select {
		case reports <- rep:
		default:
		}
		w.Write([]byte(`{}`))
	}))
	defer gateway.Close()

	s := newSender(t, gateway.URL, jellyfin.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	var rep Report
	select {
	case rep = <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat received")
	}
	cancel()
	<-done

	if gotAuth != "Bearer as_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if rep.AgentVersion != "v0.1.0-test" {
		t.Errorf("agent_version = %q", rep.AgentVersion)
	}
	if !rep.Jellyfin.Reachable || rep.Jellyfin.Version != "10.10.3" {
		t.Errorf("jellyfin status = %+v, want reachable 10.10.3", rep.Jellyfin)
	}
	if !rep.Service.Reachable || rep.Service.Type != "jellyfin" || rep.Service.Version != "10.10.3" {
		t.Errorf("service status = %+v, want reachable jellyfin 10.10.3", rep.Service)
	}
	if rep.Tunnel.Up {
		t.Errorf("tunnel.up = true for a tunnel that never came up")
	}
}

func TestHeartbeatOmitsTunnelInExternalMode(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		select {
		case bodies <- body:
		default:
		}
		w.Write([]byte(`{}`))
	}))
	defer gateway.Close()

	s := newSender(t, gateway.URL, "http://127.0.0.1:1")
	s.Tunnel = nil // external mode: user brings their own WireGuard
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case body := <-bodies:
		if _, ok := body["tunnel"]; ok {
			t.Errorf("heartbeat body contains tunnel block in external mode: %v", body)
		}
		if _, ok := body["jellyfin"]; !ok {
			t.Errorf("heartbeat body missing jellyfin block: %v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat received")
	}
}

func TestBeatAdjustsInterval(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"heartbeat_interval_seconds": 120}`))
	}))
	defer gateway.Close()

	s := newSender(t, gateway.URL, "http://127.0.0.1:1")
	if err := s.beat(context.Background()); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if s.Interval != 120*time.Second {
		t.Errorf("interval = %v, want gateway-adjusted 120s", s.Interval)
	}
}

func TestHeartbeatReportsJellyfinDown(t *testing.T) {
	jellyfin := httptest.NewServer(nil)
	jellyfin.Close() // guaranteed-refused address

	reports := make(chan Report, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		json.NewDecoder(r.Body).Decode(&rep)
		select {
		case reports <- rep:
		default:
		}
		w.Write([]byte(`{}`))
	}))
	defer gateway.Close()

	s := newSender(t, gateway.URL, jellyfin.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case rep := <-reports:
		if rep.Jellyfin.Reachable {
			t.Error("jellyfin.reachable = true, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat received")
	}
}

func TestHeartbeatStopsWhenRevoked(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"invalid_agent","message":"key regenerated"}}`))
	}))
	defer gateway.Close()

	s := newSender(t, gateway.URL, "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Run(ctx)
	if !errors.Is(err, ErrAgentRevoked) {
		t.Fatalf("Run = %v, want ErrAgentRevoked", err)
	}
}

func TestHeartbeatSurvivesTransientErrors(t *testing.T) {
	var calls int
	reports := make(chan struct{}, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		reports <- struct{}{}
		w.Write([]byte(`{}`))
	}))
	defer gateway.Close()

	s := newSender(t, gateway.URL, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case <-reports:
		// second beat arrived: the 502 didn't kill the loop
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat loop did not survive a transient error")
	}
}

// run_speedtest in a heartbeat response fires the callback; ordinary
// responses don't.
func TestHeartbeatSpeedtestRequest(t *testing.T) {
	responses := make(chan string, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case body := <-responses:
			w.Write([]byte(body))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer gateway.Close()
	responses <- `{"heartbeat_interval_seconds": 1, "run_speedtest": true}`
	responses <- `{"heartbeat_interval_seconds": 1}`

	fired := make(chan struct{}, 4)
	s := newSender(t, gateway.URL, "http://127.0.0.1:1")
	s.OnSpeedtestRequest = func() { fired <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("callback not fired for run_speedtest response")
	}
	// The follow-up response has no run_speedtest — no second firing.
	select {
	case <-fired:
		t.Fatal("callback fired for a response without run_speedtest")
	case <-time.After(300 * time.Millisecond):
	}
}
