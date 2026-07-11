package config

import (
	"log/slog"
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
		"--jellyfin-url", "http://media:8096",
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

func TestServiceHostPort(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"http://127.0.0.1:8096", "127.0.0.1:8096"},
		{"http://media", "media:80"},
		{"https://media", "media:443"},
		{"http://192.168.1.5:8920", "192.168.1.5:8920"},
	}
	for _, tt := range tests {
		c := &Config{ServiceURL: tt.url}
		got, err := c.ServiceHostPort()
		if err != nil {
			t.Errorf("ServiceHostPort(%q): %v", tt.url, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ServiceHostPort(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}

	c := &Config{ServiceURL: "::bogus::"}
	if _, err := c.ServiceHostPort(); err == nil {
		t.Error("ServiceHostPort accepted bogus URL")
	}
}

// Both the new --service-url flag and the legacy --jellyfin-url alias (and
// their env vars) land in ServiceURL; TUNNELO_SERVICE_URL wins over
// TUNNELO_JELLYFIN_URL.
func TestServiceURLAliases(t *testing.T) {
	cfg, err := Load([]string{"--service-url", "http://nas:4533"})
	if err != nil || cfg.ServiceURL != "http://nas:4533" {
		t.Errorf("service-url flag: %+v, %v", cfg, err)
	}
	cfg, err = Load([]string{"--jellyfin-url", "http://media:8096"})
	if err != nil || cfg.ServiceURL != "http://media:8096" {
		t.Errorf("jellyfin-url alias: %+v, %v", cfg, err)
	}

	t.Setenv("TUNNELO_JELLYFIN_URL", "http://old:8096")
	cfg, _ = Load(nil)
	if cfg.ServiceURL != "http://old:8096" {
		t.Errorf("legacy env: ServiceURL = %q", cfg.ServiceURL)
	}
	t.Setenv("TUNNELO_SERVICE_URL", "http://new:4533")
	cfg, _ = Load(nil)
	if cfg.ServiceURL != "http://new:4533" {
		t.Errorf("modern env should win: ServiceURL = %q", cfg.ServiceURL)
	}
}
