package mcpserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A handler that panics with http.ErrAbortHandler after streaming part of a body
// is aborting the response on purpose (net/http's documented way to signal a cut
// stream). Recovery must let it through, so the client's read fails instead of
// seeing a clean body followed by a 500 text tail.
// Mutation: drop the ErrAbortHandler re-panic in Recovery -> RED.
const streamedBytes = 4800 // 100 ms of 24 kHz 16-bit mono PCM

func TestRecoveryRepanicsErrAbortHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(make([]byte, streamedBytes))
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	srv := httptest.NewServer(Recovery(logger)(inner))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Errorf("read of a cut stream succeeded (%d bytes); want an error", len(body))
	}
	if strings.Contains(string(body), "internal server error") {
		t.Errorf("body carries a 500 text tail after the stream")
	}
	if strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("an intentional abort was logged as a crash: %s", buf.String())
	}
}
