package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/store"
)

type Handler struct {
	store *store.Store
	cache *cache.Client
}

func New(st *store.Store, c *cache.Client) *Handler { return &Handler{store: st, cache: c} }

func (h *Handler) Check(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	overall := "ok"

	if err := h.store.Ping(ctx); err != nil {
		checks["postgres"] = "error: " + err.Error()
		overall = "degraded"
	} else {
		checks["postgres"] = "ok"
	}

	if err := h.cache.Ping(ctx); err != nil {
		checks["redis"] = "error: " + err.Error()
		overall = "degraded"
	} else {
		checks["redis"] = "ok"
	}

	code := http.StatusOK
	if overall != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"status":    overall,
		"timestamp": time.Now().UTC(),
		"checks":    checks,
	})
}
