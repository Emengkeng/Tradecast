package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mt4signal/internal/signal"
	"github.com/mt4signal/internal/store"
)

type Handler struct {
	token   string
	baseURL string
	client  *http.Client
	store   *store.Store
}

func New(token, baseURL string, st *store.Store) *Handler {
	return &Handler{
		token:   token,
		baseURL: baseURL,
		store:   st,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (h *Handler) Handle(ctx context.Context, job *signal.Job) error {
	if h.token == "" {
		return nil
	}
	subs, err := h.store.ListActiveSubscribers(ctx)
	if err != nil {
		return fmt.Errorf("list subscribers: %w", err)
	}

	var lastErr error
	for _, sub := range subs {
		if sub.Channel != "telegram" {
			continue
		}
		chatID, _ := sub.Config["chat_id"].(string)
		if chatID == "" {
			continue
		}
		logID, _ := h.store.InsertDeliveryLog(ctx, job.SignalID, sub.ID, "telegram")
		if err := h.send(ctx, chatID, formatMessage(job.Signal)); err != nil {
			lastErr = err
			h.store.MarkFailed(ctx, logID, err.Error())
		} else {
			h.store.MarkDelivered(ctx, logID)
		}
	}
	return lastErr
}

func (h *Handler) send(ctx context.Context, chatID, text string) error {
	body, _ := json.Marshal(map[string]string{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	})
	url := fmt.Sprintf("%s/bot%s/sendMessage", h.baseURL, h.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram status %d", resp.StatusCode)
	}
	return nil
}

func formatMessage(sig signal.InboundSignal) string {
	e := typeEmoji(sig.SignalType)
	d := dirEmoji(sig.Direction)
	m := fmt.Sprintf("%s *%s*\n%s *%s %s*\n💰 `%.5f`  📦 `%.2f` lot",
		e, sig.SignalType, d, sig.Direction, sig.Symbol, sig.Price, sig.Lot)
	if sig.SL != nil {
		m += fmt.Sprintf("\n🛑 SL `%.5f`", *sig.SL)
	}
	if sig.TP != nil {
		m += fmt.Sprintf("  🎯 TP `%.5f`", *sig.TP)
	}
	m += fmt.Sprintf("\n🎫 `%d`  🕐 `%s`", sig.TicketID, sig.Timestamp.Format("15:04:05"))
	return m
}

func TypeEmoji(t signal.SignalType) string { return typeEmoji(t) }

func typeEmoji(t signal.SignalType) string {
	switch t {
	case signal.SignalOpen:
		return "🟢"
	case signal.SignalClose:
		return "🔴"
	case signal.SignalModify:
		return "🔵"
	case signal.SignalPartial:
		return "🟡"
	}
	return "⚪"
}

func dirEmoji(d string) string {
	if d == "BUY" {
		return "📈"
	}
	return "📉"
}
