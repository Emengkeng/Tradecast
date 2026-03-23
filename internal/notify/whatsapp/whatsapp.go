package whatsapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mt4signal/internal/config"
	"github.com/mt4signal/internal/notify/telegram"
	"github.com/mt4signal/internal/signal"
	"github.com/mt4signal/internal/store"
)

type Handler struct {
	cfg    config.NotifyConfig
	client *http.Client
	store  *store.Store
}

func New(cfg config.NotifyConfig, st *store.Store) *Handler {
	return &Handler{cfg: cfg, store: st, client: &http.Client{Timeout: 15 * time.Second}}
}

func (h *Handler) Handle(ctx context.Context, job *signal.Job) error {
	subs, err := h.store.ListActiveSubscribers(ctx)
	if err != nil {
		return fmt.Errorf("list subscribers: %w", err)
	}

	msg := formatPlain(job.Signal)
	var lastErr error
	for _, sub := range subs {
		if sub.Channel != "whatsapp" {
			continue
		}
		phone, _ := sub.Config["phone"].(string)
		if phone == "" {
			continue
		}
		logID, _ := h.store.InsertDeliveryLog(ctx, job.SignalID, sub.ID, "whatsapp")
		var err error
		switch h.cfg.WhatsAppProvider {
		case "twilio":
			err = h.twilio(ctx, phone, msg)
		case "callmebot":
			err = h.callmebot(ctx, phone, msg)
		default:
			err = fmt.Errorf("unknown provider: %s", h.cfg.WhatsAppProvider)
		}
		if err != nil {
			lastErr = err
			h.store.MarkFailed(ctx, logID, err.Error())
		} else {
			h.store.MarkDelivered(ctx, logID)
		}
	}
	return lastErr
}

func (h *Handler) twilio(ctx context.Context, to, msg string) error {
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", h.cfg.TwilioSID)
	data := url.Values{}
	data.Set("From", "whatsapp:"+h.cfg.TwilioFrom)
	data.Set("To", "whatsapp:"+to)
	data.Set("Body", msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	creds := base64.StdEncoding.EncodeToString([]byte(h.cfg.TwilioSID + ":" + h.cfg.TwilioToken))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("twilio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("twilio %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (h *Handler) callmebot(ctx context.Context, phone, msg string) error {
	params := url.Values{}
	params.Set("phone", phone)
	params.Set("text", msg)
	params.Set("apikey", h.cfg.CallMeBotAPIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.callmebot.com/whatsapp.php?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("callmebot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("callmebot %d", resp.StatusCode)
	}
	return nil
}

func formatPlain(sig signal.InboundSignal) string {
	e := telegram.TypeEmoji(sig.SignalType)
	lines := []string{
		fmt.Sprintf("%s %s Signal", e, sig.SignalType),
		fmt.Sprintf("%s %s", sig.Direction, sig.Symbol),
		fmt.Sprintf("Price: %.5f  Lot: %.2f", sig.Price, sig.Lot),
	}
	if sig.SL != nil {
		lines = append(lines, fmt.Sprintf("SL: %.5f", *sig.SL))
	}
	if sig.TP != nil {
		lines = append(lines, fmt.Sprintf("TP: %.5f", *sig.TP))
	}
	lines = append(lines, fmt.Sprintf("Ticket: %d", sig.TicketID))
	return strings.Join(lines, "\n")
}
