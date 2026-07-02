// Package detect probes a local Jellyfin server via its unauthenticated
// /System/Info/Public endpoint.
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

// DefaultURL is where Jellyfin listens out of the box.
const DefaultURL = "http://127.0.0.1:8096"

// Info is the subset of /System/Info/Public the agent reports.
type Info struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

// Prober checks whether a Jellyfin server is reachable at BaseURL.
type Prober struct {
	BaseURL    string
	HTTPClient *http.Client
}

// New returns a Prober for the given base URL (DefaultURL when empty).
func New(baseURL string) *Prober {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	return &Prober{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Probe queries /System/Info/Public. A nil error means Jellyfin answered
// with a well-formed public info document.
func (p *Prober) Probe(ctx context.Context) (*Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/System/Info/Public", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin unreachable at %s: %w", p.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jellyfin at %s returned status %d", p.BaseURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading jellyfin response: %w", err)
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
