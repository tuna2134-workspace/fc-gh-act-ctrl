package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v88/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureSink struct {
	called     bool
	action     string
	repo       string
	jobID      int64
	deliveryID string
	err        error
}

func (s *captureSink) HandleWorkflowJob(_ context.Context, event *github.WorkflowJobEvent, deliveryID string) error {
	s.called = true
	s.action = event.GetAction()
	s.jobID = event.WorkflowJob.GetID()
	s.repo = event.Repo.GetFullName()
	s.deliveryID = deliveryID
	return s.err
}

const payload = `{
  "action": "queued",
  "workflow_job": {"id": 42, "labels": ["self-hosted", "fcghact"], "status": "queued"},
  "repository": {"name": "repo", "full_name": "owner/repo", "owner": {"login": "owner"}},
  "installation": {"id": 99}
}`

func sign(secret []byte, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newRequest(body []byte, secret []byte, eventType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventType)
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	return req
}

func TestHandlerAcceptsSignedWorkflowJob(t *testing.T) {
	secret := []byte("secret")
	sink := &captureSink{}
	h := New(secret, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest([]byte(payload), secret, "workflow_job"))

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.True(t, sink.called)
	assert.Equal(t, "queued", sink.action)
	assert.Equal(t, int64(42), sink.jobID)
	assert.Equal(t, "owner/repo", sink.repo)
	assert.Equal(t, "delivery-1", sink.deliveryID)
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	sink := &captureSink{}
	h := New([]byte("secret"), sink, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := newRequest([]byte(payload), []byte("wrong"), "workflow_job")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, sink.called)
}

func TestHandlerIgnoresOtherEvents(t *testing.T) {
	sink := &captureSink{}
	h := New([]byte("secret"), sink, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest([]byte(payload), []byte("secret"), "push"))

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, sink.called)
}

func TestHandlerRejectsNonPost(t *testing.T) {
	h := New([]byte("secret"), &captureSink{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHandlerReportsSinkFailure(t *testing.T) {
	sink := &captureSink{err: assert.AnError}
	h := New([]byte("secret"), sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest([]byte(payload), []byte("secret"), "workflow_job"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
