// Package config collects agent settings from flags and environment
// variables (flags win). Every option has a TUNNELO_* variable so the
// Docker/Unraid deployments need no arguments.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"

	"github.com/abiteman/tunnelo-agent/internal/register"
)

// Config holds all agent settings.
type Config struct {
	Token      string // one-time user token; only needed until first registration
	GatewayURL string
	StateDir   string
	// ServiceURL is the local HTTP service the tunnel exposes (Jellyfin by
	// default, but any HTTP service works). TUNNELO_JELLYFIN_URL remains a
	// supported alias.
	ServiceURL string
	// HealthPath is the path probed for generic reachability ("/" default);
	// any HTTP answer counts as up.
	HealthPath string
	// ServiceType optionally names the service ("jellyfin", "navidrome", …)
	// for dashboard display; empty = autodetect (jellyfin probe, else "http").
	ServiceType string
	Interface   string
	MTU         int    // 0 = use the registration's value (gateway-chosen)
	TunnelMode  string // register.TunnelManaged or register.TunnelExternal
	WgConfigOut string // where external mode writes the wg-quick config
	Userspace   bool   // force userspace wireguard-go
	Speedtest   bool   // re-run the upload test even if already done
	LogLevel    slog.Level
}

const defaultGatewayURL = "https://api.tunnelo.io"

// Load parses args (excluding the program name) into a Config.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("tunnelo-agent", flag.ContinueOnError)
	cfg := &Config{}
	var logLevel string

	fs.StringVar(&cfg.Token, "token", envStr("TUNNELO_TOKEN", ""),
		"one-time user token from the Tunnelo dashboard (env TUNNELO_TOKEN)")
	fs.StringVar(&cfg.GatewayURL, "gateway-url", envStr("TUNNELO_GATEWAY_URL", defaultGatewayURL),
		"gateway API base URL (env TUNNELO_GATEWAY_URL)")
	fs.StringVar(&cfg.StateDir, "state-dir", envStr("TUNNELO_STATE_DIR", "/var/lib/tunnelo-agent"),
		"directory for persisted state, including the WireGuard private key (env TUNNELO_STATE_DIR)")
	serviceDefault := envStr("TUNNELO_SERVICE_URL", envStr("TUNNELO_JELLYFIN_URL", "http://127.0.0.1:8096"))
	fs.StringVar(&cfg.ServiceURL, "service-url", serviceDefault,
		"where to reach the local service the tunnel exposes (env TUNNELO_SERVICE_URL)")
	fs.StringVar(&cfg.ServiceURL, "jellyfin-url", serviceDefault,
		"deprecated alias for --service-url (env TUNNELO_JELLYFIN_URL)")
	fs.StringVar(&cfg.HealthPath, "health-path", envStr("TUNNELO_HEALTH_PATH", "/"),
		"path probed for reachability; any HTTP answer counts as up (env TUNNELO_HEALTH_PATH)")
	fs.StringVar(&cfg.ServiceType, "service-type", envStr("TUNNELO_SERVICE_TYPE", ""),
		"service name for dashboard display, e.g. jellyfin, navidrome; empty = autodetect (env TUNNELO_SERVICE_TYPE)")
	fs.StringVar(&cfg.Interface, "interface", envStr("TUNNELO_INTERFACE", "tunnelo0"),
		"WireGuard interface name (env TUNNELO_INTERFACE)")
	fs.IntVar(&cfg.MTU, "mtu", envInt("TUNNELO_MTU"),
		"tunnel MTU override; 0 uses the gateway-provided value (env TUNNELO_MTU)")
	fs.StringVar(&cfg.TunnelMode, "tunnel-mode", envStr("TUNNELO_TUNNEL_MODE", register.TunnelManaged),
		"'managed' runs WireGuard itself; 'external' writes a wg-quick config for WireGuard you already run (env TUNNELO_TUNNEL_MODE)")
	fs.StringVar(&cfg.WgConfigOut, "wg-config-out", envStr("TUNNELO_WG_CONFIG_OUT", ""),
		"external mode: where to write the wg-quick config (default <state-dir>/tunnelo-wg.conf) (env TUNNELO_WG_CONFIG_OUT)")
	fs.BoolVar(&cfg.Userspace, "userspace", envBool("TUNNELO_USERSPACE"),
		"use userspace wireguard-go even if the kernel module is available (env TUNNELO_USERSPACE)")
	fs.BoolVar(&cfg.Speedtest, "speedtest", false,
		"re-run the upload speed test on startup")
	fs.StringVar(&logLevel, "log-level", envStr("TUNNELO_LOG_LEVEL", "info"),
		"log level: debug, info, warn, error (env TUNNELO_LOG_LEVEL)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", logLevel)
	}
	if _, err := url.ParseRequestURI(cfg.GatewayURL); err != nil {
		return nil, fmt.Errorf("invalid gateway URL %q", cfg.GatewayURL)
	}
	if cfg.TunnelMode != register.TunnelManaged && cfg.TunnelMode != register.TunnelExternal {
		return nil, fmt.Errorf("invalid tunnel mode %q (want %q or %q)",
			cfg.TunnelMode, register.TunnelManaged, register.TunnelExternal)
	}
	if cfg.MTU != 0 && (cfg.MTU < 576 || cfg.MTU > 1500) {
		return nil, fmt.Errorf("invalid MTU %d (want 576-1500, or 0 for the gateway-provided value)", cfg.MTU)
	}
	return cfg, nil
}

// ServiceHostPort returns the host:port the tunnel relay should dial.
func (c *Config) ServiceHostPort() (string, error) {
	u, err := url.Parse(c.ServiceURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid service URL %q", c.ServiceURL)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	return u.Host + ":" + port, nil
}

func envStr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	return err == nil && v
}

func envInt(key string) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return 0
	}
	return n
}
