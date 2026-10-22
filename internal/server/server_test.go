package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
)

func testServer(t *testing.T, ready func(context.Context) error) http.Handler {
	t.Helper()
	cfg := config.ServerConfig{
		Listen:       "127.0.0.1:0",
		WebhookPath:  "/webhook",
		HealthPath:   "/healthz",
		ReadyPath:    "/readyz",
		MetricsPath:  "/metrics",
		ReadTimeout:  config.Duration(5 * time.Second),
		WriteTimeout: config.Duration(5 * time.Second),
	}
	srv := New(Options{
		Config: cfg,
		Webhook: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}),
		Ready:  ready,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return srv.Handler
}

func TestHealthEndpoints(t *testing.T) {
	h := testServer(t, func(context.Context) error { return nil })

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, path)
	}
}

func TestReadyReportsFailure(t *testing.T) {
	h := testServer(t, func(context.Context) error { return errors.New("no kvm") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestWebhookRouteAndMethod(t *testing.T) {
	h := testServer(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", nil))
	assert.Equal(t, http.StatusAccepted, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
