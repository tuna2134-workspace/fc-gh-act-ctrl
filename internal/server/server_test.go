package server

import (
	"bytes"
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

func TestRequestLoggerSuppressesScannerPaths(t *testing.T) {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	var infoBuf bytes.Buffer
	infoLogger := slog.New(slog.NewJSONHandler(&infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	requestLogger(infoLogger, "/webhook")(notFound).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/.env", nil))
	assert.Empty(t, infoBuf.String(), "scanner 404s must not be logged at INFO")

	var debugBuf bytes.Buffer
	debugLogger := slog.New(slog.NewJSONHandler(&debugBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	requestLogger(debugLogger, "/webhook")(notFound).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/.env", nil))
	assert.Contains(t, debugBuf.String(), `"level":"DEBUG"`)
	assert.Contains(t, debugBuf.String(), `"/.env"`)
}

func TestRequestLoggerLogsWebhookFailures(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	requestLogger(logger, "/webhook")(unauthorized).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhook", nil))

	assert.Contains(t, buf.String(), `"level":"WARN"`)
	assert.Contains(t, buf.String(), `"status":401`)
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
