package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mt4signal/internal/signal"
	"github.com/mt4signal/internal/store"
)

type Handler struct {
	client *http.Client
	store  *store.Store
}

func New(st *store.Store) *Handler {
	return &Handler{store: st, client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *Handler) Handle(ctx context.Context, job *signal.Job) error {
	subs, err := h.store.ListActiveSubscribers(ctx)
	if err != nil {
		return fmt.Errorf("list subscribers: %w", err)
	}

	payload := map[string]any{
		"signal_id": job.SignalID,
		"signal":    job.Signal,
		"sent_at":   time.Now(),
	}
	body, _ := json.Marshal(payload)

	var lastErr error
	for _, sub := range subs {
		if sub.Channel != "webhook" {
			continue
		}
		webhookURL, _ := sub.Config["url"].(string)
		if webhookURL == "" {
			continue
		}
		secret, _ := sub.Config["secret"].(string)
		logID, _ := h.store.InsertDeliveryLog(ctx, job.SignalID, sub.ID, "webhook")
		if err := h.send(ctx, webhookURL, secret, body); err != nil {
			lastErr = err
			h.store.MarkFailed(ctx, logID, err.Error())
		} else {
			h.store.MarkDelivered(ctx, logID)
		}
	}
	return lastErr
}

func (h *Handler) send(ctx context.Context, url, secret string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "MT4Signal/1.0")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-MT4Signal-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}
