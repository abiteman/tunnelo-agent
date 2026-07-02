// Package speedtest measures upload throughput to the gateway and reports
// the result. Residential upload bandwidth is the real streaming ceiling for
// most users; measuring it at onboarding sets expectations before the first
// buffering complaint.
package speedtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/abiteman/tunnelo-agent/internal/register"
)

const (
	defaultSizeBytes  = 32 << 20 // 32 MiB
	defaultMaxSeconds = 15
	chunkSize         = 1 << 20
)

// Result is what gets reported to the gateway.
type Result struct {
	UploadMbps float64   `json:"upload_mbps"`
	BytesSent  int64     `json:"bytes_sent"`
	DurationMS int64     `json:"duration_ms"`
	MeasuredAt time.Time `json:"measured_at"`
}

// Runner uploads random data to the gateway's sink endpoint and reports the
// measured throughput.
type Runner struct {
	GatewayURL  string // gateway API base URL
	AgentID     string
	AgentSecret string
	Config      register.SpeedtestConfig
	HTTPClient  *http.Client
}

// Run measures upload throughput and reports it. It returns the result even
// when reporting fails, so callers can still log it.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	result, err := r.measure(ctx)
	if err != nil {
		return nil, fmt.Errorf("measuring upload: %w", err)
	}
	if err := r.report(ctx, result); err != nil {
		return result, fmt.Errorf("reporting result: %w", err)
	}
	return result, nil
}

// countingReader serves random bytes and records how many were consumed,
// so a test cut short by the deadline still yields an accurate sample.
type countingReader struct {
	remaining int64
	sent      int64
	chunk     []byte
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > c.remaining {
		n = c.remaining
	}
	if n > int64(len(c.chunk)) {
		n = int64(len(c.chunk))
	}
	copy(p, c.chunk[:n])
	c.remaining -= n
	c.sent += n
	return int(n), nil
}

func (r *Runner) measure(ctx context.Context) (*Result, error) {
	size := r.Config.SizeBytes
	if size <= 0 {
		size = defaultSizeBytes
	}
	maxSeconds := r.Config.MaxSeconds
	if maxSeconds <= 0 {
		maxSeconds = defaultMaxSeconds
	}
	if r.Config.UploadURL == "" {
		return nil, fmt.Errorf("no upload URL configured")
	}

	chunk := make([]byte, chunkSize)
	if _, err := rand.Read(chunk); err != nil {
		return nil, err
	}
	body := &countingReader{remaining: size, chunk: chunk}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Config.UploadURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+r.AgentSecret)
	req.ContentLength = size

	start := time.Now()
	resp, err := r.httpClient().Do(req)
	elapsed := time.Since(start)
	// A deadline hit mid-upload is a valid (truncated) sample, not a failure.
	if err != nil && !(ctx.Err() != nil && body.sent > 0) {
		return nil, err
	}
	if resp != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("upload sink returned status %d", resp.StatusCode)
		}
	}
	if body.sent == 0 || elapsed <= 0 {
		return nil, fmt.Errorf("no data uploaded")
	}

	mbps := float64(body.sent) * 8 / elapsed.Seconds() / 1e6
	return &Result{
		UploadMbps: mbps,
		BytesSent:  body.sent,
		DurationMS: elapsed.Milliseconds(),
		MeasuredAt: time.Now().UTC(),
	}, nil
}

func (r *Runner) report(ctx context.Context, result *Result) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	url := strings.TrimRight(r.GatewayURL, "/") + "/v1/agents/" + r.AgentID + "/speedtest"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.AgentSecret)

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return register.ParseErrorBody(resp.StatusCode, body)
	}
	return nil
}

func (r *Runner) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	// No overall client timeout: the upload duration is bounded by the
	// per-request context instead.
	return http.DefaultClient
}
