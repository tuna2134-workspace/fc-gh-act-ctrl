// Package webhook verifies and decodes GitHub webhook deliveries.
package webhook

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/go-github/v88/github"
)

// Sink receives decoded workflow_job events.
type Sink interface {
	HandleWorkflowJob(ctx context.Context, event *github.WorkflowJobEvent, deliveryID string) error
}

// Handler is an http.Handler for the GitHub webhook endpoint.
type Handler struct {
	secret  []byte
	sink    Sink
	log     *slog.Logger
	maxBody int64
}

// New returns a webhook handler. secret is the GitHub App webhook secret.
func New(secret []byte, sink Sink, log *slog.Logger) *Handler {
	return &Handler{
		secret:  secret,
		sink:    sink,
		log:     log,
		maxBody: 10 << 20,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	payload, err := github.ValidatePayload(r, h.secret)
	if err != nil {
		// Do not echo the validation error: it can reveal whether the delivery
		// was signed at all.
		h.log.Warn("webhook signature validation failed", "remote", r.RemoteAddr, "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := github.WebHookType(r)
	deliveryID := github.DeliveryID(r)
	if eventType != "workflow_job" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	parsed, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		h.log.Warn("webhook payload parse failed", "delivery", deliveryID, "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	event, ok := parsed.(*github.WorkflowJobEvent)
	if !ok {
		h.log.Warn("unexpected webhook payload type", "delivery", deliveryID, "type", eventType)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if err := h.sink.HandleWorkflowJob(r.Context(), event, deliveryID); err != nil {
		h.log.Error("handle workflow_job failed", "delivery", deliveryID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
