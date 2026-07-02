package speedtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abiteman/tunnelo-agent/internal/register"
)

func TestRunMeasuresAndReports(t *testing.T) {
	const size = 4 << 20

	var sinkBytes int64
	var sinkAuth string
	var reported Result
	reportCalled := false

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sink", func(w http.ResponseWriter, r *http.Request) {
		sinkAuth = r.Header.Get("Authorization")
		n, _ := io.Copy(io.Discard, r.Body)
		sinkBytes = n
	})
	mux.HandleFunc("POST /v1/agents/agt_1/speedtest", func(w http.ResponseWriter, r *http.Request) {
		reportCalled = true
		if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
			t.Errorf("decoding report: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runner := &Runner{
		GatewayURL:  srv.URL,
		AgentID:     "agt_1",
		AgentSecret: "as_secret",
		Config: register.SpeedtestConfig{
			UploadURL:  srv.URL + "/sink",
			SizeBytes:  size,
			MaxSeconds: 10,
		},
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sinkBytes != size {
		t.Errorf("sink received %d bytes, want %d", sinkBytes, size)
	}
	if sinkAuth != "Bearer as_secret" {
		t.Errorf("sink Authorization = %q", sinkAuth)
	}
	if result.BytesSent != size || result.UploadMbps <= 0 {
		t.Errorf("result = %+v", result)
	}
	if !reportCalled {
		t.Fatal("result was not reported")
	}
	if reported.BytesSent != result.BytesSent || reported.UploadMbps != result.UploadMbps {
		t.Errorf("reported %+v, measured %+v", reported, result)
	}
	if reported.MeasuredAt.IsZero() {
		t.Error("reported measured_at is zero")
	}
}

func TestRunReturnsResultWhenReportFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sink", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	})
	mux.HandleFunc("POST /v1/agents/agt_1/speedtest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"code":"unavailable","message":"try later"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runner := &Runner{
		GatewayURL:  srv.URL,
		AgentID:     "agt_1",
		AgentSecret: "s",
		Config:      register.SpeedtestConfig{UploadURL: srv.URL + "/sink", SizeBytes: 1 << 20},
	}
	result, err := runner.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded despite failed report")
	}
	if result == nil || result.BytesSent != 1<<20 {
		t.Errorf("result = %+v, want measurement despite report failure", result)
	}
}

func TestRunSinkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	runner := &Runner{
		GatewayURL: srv.URL,
		AgentID:    "agt_1",
		Config:     register.SpeedtestConfig{UploadURL: srv.URL, SizeBytes: 1 << 20},
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded despite sink rejecting upload")
	}
}
