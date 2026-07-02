package detect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeSuccess(t *testing.T) {
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

	info, err := New(srv.URL).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Version != "10.10.3" || info.ServerName != "Living Room" || info.ID != "f3a1b2c3" {
		t.Errorf("info = %+v", info)
	}
}

func TestProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close() // guaranteed-refused address

	if _, err := New(srv.URL).Probe(context.Background()); err == nil {
		t.Fatal("Probe succeeded against closed server")
	}
}

func TestProbeNonJellyfin(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"html page", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>hello</html>"))
		}},
		{"empty json", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		}},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			if _, err := New(srv.URL).Probe(context.Background()); err == nil {
				t.Fatal("Probe succeeded on non-Jellyfin response")
			}
		})
	}
}

func TestNewDefaultsAndTrimsSlash(t *testing.T) {
	if p := New(""); p.BaseURL != DefaultURL {
		t.Errorf("default BaseURL = %q", p.BaseURL)
	}
	if p := New("http://media:8096/"); p.BaseURL != "http://media:8096" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", p.BaseURL)
	}
}
