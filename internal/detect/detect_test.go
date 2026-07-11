package detect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeJellyfin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{
			"LocalAddress": "http://192.168.1.10:8096",
			"ServerName": "Living Room",
			"Version": "10.10.3",
			"ProductName": "Jellyfin Server",
			"Id": "f3a1b2c3"
		}`))
	}))
	defer srv.Close()

	res := New(srv.URL, "", "").Probe(context.Background())
	if !res.Reachable || res.Type != "jellyfin" {
		t.Fatalf("res = %+v, want reachable jellyfin", res)
	}
	if res.Version != "10.10.3" || res.ServerName != "Living Room" || res.ID != "f3a1b2c3" {
		t.Errorf("res = %+v", res)
	}
}

func TestProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close() // guaranteed-refused address

	res := New(srv.URL, "", "").Probe(context.Background())
	if res.Reachable || res.Err == nil {
		t.Fatalf("res = %+v, want unreachable with error", res)
	}
}

// Non-Jellyfin services are reachable as generic HTTP — any status code
// counts, including auth challenges and 404s.
func TestProbeGenericHTTP(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"html page", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>hello</html>"))
		}},
		{"auth challenge", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}},
		{"not found", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			res := New(srv.URL, "", "").Probe(context.Background())
			if !res.Reachable || res.Type != "http" {
				t.Fatalf("res = %+v, want reachable http", res)
			}
			if res.Version != "" {
				t.Errorf("generic probe should not report a version: %+v", res)
			}
		})
	}
}

// An operator-declared type wins over autodetection, and the Jellyfin
// enrichment still applies when the probe succeeds.
func TestProbeDeclaredType(t *testing.T) {
	generic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer generic.Close()
	if res := New(generic.URL, "", "navidrome").Probe(context.Background()); !res.Reachable || res.Type != "navidrome" {
		t.Fatalf("res = %+v, want reachable navidrome", res)
	}
}

// A custom health path is used for the generic probe.
func TestProbeHealthPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Refuse everything else at the TCP-ish level we can simulate:
		// hijack and close without a response.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
	}))
	defer srv.Close()

	if res := New(srv.URL, "ping", "").Probe(context.Background()); !res.Reachable {
		t.Fatalf("res = %+v, want reachable via /ping", res)
	}
}

func TestNewDefaultsAndTrimsSlash(t *testing.T) {
	if p := New("", "", ""); p.BaseURL != DefaultURL || p.HealthPath != "/" {
		t.Errorf("defaults = %q %q", p.BaseURL, p.HealthPath)
	}
	if p := New("http://media:8096/", "status", ""); p.BaseURL != "http://media:8096" || p.HealthPath != "/status" {
		t.Errorf("got %q %q, want trimmed base and leading-slash path", p.BaseURL, p.HealthPath)
	}
}
