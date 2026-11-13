package kubeconfig

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDebuggingRoundTripper(t *testing.T) {
	mockTransport := http.DefaultTransport
	mockLogger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	traceID := "test-trace"

	roundTripper := newDebuggingRoundTripper(mockLogger, traceID)(mockTransport)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL, nil)
	req.Header.Set("X-Trace-ID", traceID)

	resp, err := roundTripper.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if req.Header.Get("X-Trace-ID") != traceID {
		t.Errorf("expected trace ID %s, got %s", traceID, req.Header.Get("X-Trace-ID"))
	}
}
