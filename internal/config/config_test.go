package config

import (
	"log/slog"
	"strconv"
	"testing"

	"github.com/abiteman/tunnelo-agent/internal/register"
)

func TestLoadDefaultsAndFlags(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != defaultGatewayURL || cfg.Interface != "tunnelo0" {
		t.Errorf("defaults = %+v", cfg)
	}

	cfg, err = Load([]string{
		"--token", "tok",
		"--gateway-url", "https://gw.example.com",
		"--service-url", "http://media:8096",
		"--userspace",
		"--log-level", "debug",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "tok" || cfg.GatewayURL != "https://gw.example.com" || !cfg.Userspace {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("log level = %v", cfg.LogLevel)
	}
}

func TestLoadEnvFallback(t *testing.T) {
	t.Setenv("TUNNELO_TOKEN", "envtok")
	t.Setenv("TUNNELO_USERSPACE", "true")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "envtok" || !cfg.Userspace {
		t.Errorf("cfg = %+v, want env values applied", cfg)
	}

	// Flags beat env.
	cfg, err = Load([]string{"--token", "flagtok"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "flagtok" {
		t.Errorf("token = %q, want flag to win over env", cfg.Token)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	if _, err := Load([]string{"--log-level", "verbose"}); err == nil {
		t.Error("Load accepted bogus log level")
	}
	if _, err := Load([]string{"--gateway-url", "not a url"}); err == nil {
		t.Error("Load accepted bogus gateway URL")
	}
	if _, err := Load([]string{"--tunnel-mode", "sidecar"}); err == nil {
		t.Error("Load accepted bogus tunnel mode")
	}
}

func TestLoadTunnelModes(t *testing.T) {
	cfg, err := Load(nil)
	if err != nil || cfg.TunnelMode != register.TunnelManaged {
		t.Errorf("default tunnel mode = %q, %v; want managed", cfg.TunnelMode, err)
	}
	cfg, err = Load([]string{"--tunnel-mode", "external", "--wg-config-out", "/etc/wireguard/tunnelo.conf"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TunnelMode != register.TunnelExternal || cfg.WgConfigOut != "/etc/wireguard/tunnelo.conf" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// A single ServiceURL yields a one-entry service list (host:port derived).
func TestSingleServiceFromURL(t *testing.T) {
	tests := []struct {
		url  string
		host string
		port int
	}{
		{"http://127.0.0.1:8096", "127.0.0.1", 8096},
		{"http://media", "media", 80},
		{"https://media", "media", 443},
		{"http://192.168.1.5:8920", "192.168.1.5", 8920},
	}
	for _, tt := range tests {
		cfg, err := Load([]string{"--service-url", tt.url})
		if err != nil {
			t.Errorf("Load(%q): %v", tt.url, err)
			continue
		}
		if len(cfg.Services) != 1 {
			t.Errorf("%q: got %d services, want 1", tt.url, len(cfg.Services))
			continue
		}
		s := cfg.Services[0]
		if s.Host != tt.host || s.Port != tt.port {
			t.Errorf("%q -> %s:%d, want %s:%d", tt.url, s.Host, s.Port, tt.host, tt.port)
		}
	}
}

// TUNNELO_SERVICES parses full host:port entries and the bare-port shorthand
// (bare ports inherit the preceding host), primary first.
func TestParseServices(t *testing.T) {
	specs, err := parseServices("192.168.1.5:8096,7878,8989", "/")
	if err != nil {
		t.Fatalf("parseServices: %v", err)
	}
	want := []ServiceSpec{
		{Host: "192.168.1.5", Port: 8096},
		{Host: "192.168.1.5", Port: 7878},
		{Host: "192.168.1.5", Port: 8989},
	}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i, w := range want {
		if specs[i].Host != w.Host || specs[i].Port != w.Port {
			t.Errorf("spec[%d] = %s:%d, want %s:%d", i, specs[i].Host, specs[i].Port, w.Host, w.Port)
		}
		if specs[i].URL != "http://"+w.Host+":"+itoa(w.Port) {
			t.Errorf("spec[%d] URL = %q", i, specs[i].URL)
		}
	}

	// Mixed full host:port entries keep their own hosts.
	specs, err = parseServices("10.0.0.1:8096,10.0.0.2:7878", "/")
	if err != nil || len(specs) != 2 || specs[1].Host != "10.0.0.2" || specs[1].Port != 7878 {
		t.Fatalf("mixed hosts: %+v err=%v", specs, err)
	}

	// A leading bare port has no host to inherit.
	if _, err := parseServices("7878,8989", "/"); err == nil {
		t.Error("bare leading port should error (no preceding host:port)")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestServiceURLFromFlagAndEnv(t *testing.T) {
	cfg, err := Load([]string{"--service-url", "http://nas:4533"})
	if err != nil || cfg.ServiceURL != "http://nas:4533" {
		t.Errorf("service-url flag: %+v, %v", cfg, err)
	}

	t.Setenv("TUNNELO_SERVICE_URL", "http://new:4533")
	cfg, _ = Load(nil)
	if cfg.ServiceURL != "http://new:4533" {
		t.Errorf("env: ServiceURL = %q", cfg.ServiceURL)
	}
}

// Two services on the same port would collide (routing is keyed by port).
func TestParseServicesRejectsDuplicatePort(t *testing.T) {
	if _, err := parseServices("192.168.1.5:80,192.168.1.6:80", "/"); err == nil {
		t.Error("duplicate port should be rejected")
	}
	// Distinct ports are fine.
	if _, err := parseServices("192.168.1.5:80,192.168.1.6:81", "/"); err != nil {
		t.Errorf("distinct ports rejected: %v", err)
	}
}

// IPv6 hosts stay bracketed in the probe URL.
func TestParseServicesIPv6(t *testing.T) {
	specs, err := parseServices("[::1]:8096,7878", "/")
	if err != nil {
		t.Fatalf("parseServices: %v", err)
	}
	if specs[0].Host != "::1" || specs[0].URL != "http://[::1]:8096" {
		t.Errorf("ipv6 primary = %+v, want host ::1 URL http://[::1]:8096", specs[0])
	}
	// Bare port inherits the bracketed host.
	if specs[1].URL != "http://[::1]:7878" {
		t.Errorf("ipv6 inherited URL = %q, want http://[::1]:7878", specs[1].URL)
	}
}
