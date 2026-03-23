package mt4copier

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/signal"
	"github.com/mt4signal/internal/store"
)

type Handler struct {
	cache  *cache.Client
	store  *store.Store
	logger *slog.Logger
}

func New(c *cache.Client, st *store.Store, logger *slog.Logger) *Handler {
	return &Handler{cache: c, store: st, logger: logger}
}

func (h *Handler) Handle(ctx context.Context, job *signal.Job) error {
	subs, err := h.store.ListActiveSubscribers(ctx)
	if err != nil {
		return fmt.Errorf("list subscribers: %w", err)
	}

	var lastErr error
	for _, sub := range subs {
		if sub.Channel != "mt4" {
			continue
		}

		// Optional: filter by symbol
		if syms, ok := sub.Config["symbols"].([]any); ok && len(syms) > 0 {
			matched := false
			for _, s := range syms {
				if sym, ok := s.(string); ok && sym == job.Signal.Symbol {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		logID, _ := h.store.InsertDeliveryLog(ctx, job.SignalID, sub.ID, "mt4")
		// Signal already lives in Redis from signal handler — receiver EA polls it
		if err := h.store.MarkDelivered(ctx, logID); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
