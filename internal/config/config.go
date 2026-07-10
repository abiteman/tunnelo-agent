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
	Token       string // one-time user token; only needed until first registration
	GatewayURL  string
	StateDir    string
	JellyfinURL string
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
	fs.StringVar(&cfg.JellyfinURL, "jellyfin-url", envStr("TUNNELO_JELLYFIN_URL", "http://127.0.0.1:8096"),
		"where to reach the local Jellyfin server (env TUNNELO_JELLYFIN_URL)")
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

// JellyfinHostPort returns the host:port the tunnel relay should dial.
func (c *Config) JellyfinHostPort() (string, error) {
	u, err := url.Parse(c.JellyfinURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid Jellyfin URL %q", c.JellyfinURL)
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
