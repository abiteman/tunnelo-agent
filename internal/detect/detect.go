// Package detect probes the local service the tunnel exposes. It knows how
// to enrich the result for Jellyfin (version/name via the unauthenticated
// /System/Info/Public endpoint) but treats ANY HTTP answer on the health
// path as reachable — the bar is "would proxying work", not "is this
// Jellyfin". Auth challenges (401/403) and 404s count as reachable: the
// service answered.
package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultURL is where Jellyfin listens out of the box — kept as the default
// service for compatibility with existing installs.
const DefaultURL = "http://127.0.0.1:8096"

// Info is the subset of Jellyfin's /System/Info/Public the agent reports.
type Info struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

// Result is what one probe learned about the local service.
type Result struct {
	Reachable bool
	// Type is the operator-declared service type when configured; otherwise
	// "jellyfin" when the Jellyfin probe succeeded, else "http".
	Type string
	// Version/ServerName/ID are set only when a type-specific probe (today:
	// Jellyfin) could read them.
	Version    string
	ServerName string
	ID         string
	// Err describes why the service was unreachable (nil when Reachable).
	Err error
}

// Prober checks whether the local service is reachable at BaseURL.
type Prober struct {
	BaseURL     string
	HealthPath  string // generic probe path, default "/"
	ServiceType string // operator-declared type; "" = autodetect
	HTTPClient  *http.Client
}

// New returns a Prober for the given base URL (DefaultURL when empty).
func New(baseURL, healthPath, serviceType string) *Prober {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	if healthPath == "" {
		healthPath = "/"
	}
	if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}
	return &Prober{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		HealthPath:  healthPath,
		ServiceType: serviceType,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	}
}

// Probe checks the service. Jellyfin's public-info endpoint is tried first
// because it enriches the result (version, server name); any HTTP answer on
// HealthPath is the generic fallback.
func (p *Prober) Probe(ctx context.Context) Result {
	res := Result{Type: p.ServiceType}

	if info, err := p.probeJellyfin(ctx); err == nil {
		res.Reachable = true
		if res.Type == "" {
			res.Type = "jellyfin"
		}
		res.Version, res.ServerName, res.ID = info.Version, info.ServerName, info.ID
		return res
	}

	if err := p.probeHTTP(ctx); err != nil {
		res.Err = err
		return res
	}
	res.Reachable = true
	if res.Type == "" {
		res.Type = "http"
	}
	return res
}

// probeJellyfin queries /System/Info/Public. A nil error means Jellyfin
// answered with a well-formed public info document.
func (p *Prober) probeJellyfin(ctx context.Context) (*Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/System/Info/Public", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable at %s: %w", p.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d (not Jellyfin?)", p.BaseURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("unexpected response from %s (not Jellyfin?): %w", p.BaseURL, err)
	}
	if info.Version == "" && info.ID == "" {
		return nil, fmt.Errorf("response from %s has no Version or Id (not Jellyfin?)", p.BaseURL)
	}
	return &info, nil
}

// probeHTTP is the generic reachability check: any HTTP status counts —
// a 401 from Radarr or a 404 on / still proves the service is up and
// proxying would work. Only transport failures are unreachable.
func (p *Prober) probeHTTP(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+p.HealthPath, nil)
	if err != nil {
		return err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("service unreachable at %s: %w", p.BaseURL, err)
	}
	// Body drained minimally so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	return nil
}
