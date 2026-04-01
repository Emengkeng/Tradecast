package signal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mt4signal/internal/auth"
	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/config"
	"github.com/mt4signal/internal/store"
)

type SignalType string

const (
	SignalOpen    SignalType = "OPEN"
	SignalModify  SignalType = "MODIFY"
	SignalClose   SignalType = "CLOSE"
	SignalPartial SignalType = "PARTIAL"
)

// InboundSignal is the payload POSTed by the MT4 monitor EA.
// X-Signal-Signature header: HMAC-SHA256(secret, "ticket_id:signal_type:symbol:timestamp")
// X-Signal-Timestamp header: RFC3339 timestamp
type InboundSignal struct {
	TicketID   int64      `json:"ticket_id"`
	SignalType SignalType `json:"signal_type"`
	Symbol     string     `json:"symbol"`
	Direction  string     `json:"direction"`
	Price      float64    `json:"price"`
	SL         *float64   `json:"sl,omitempty"`
	TP         *float64   `json:"tp,omitempty"`
	Lot        float64    `json:"lot"`
	Timestamp  time.Time  `json:"timestamp"`
}

// Job is pushed onto the Redis per-symbol queue for the worker.
type Job struct {
	JobID      string        `json:"job_id"`
	SignalID   uuid.UUID     `json:"signal_id"`
	Signal     InboundSignal `json:"signal"`
	Attempts   int           `json:"attempts"`
	EnqueuedAt time.Time     `json:"enqueued_at"`
}

type Handler struct {
	store  *store.Store
	cache  *cache.Client
	cfg    config.AuthConfig
	logger *slog.Logger
}

func NewHandler(st *store.Store, c *cache.Client, cfg config.AuthConfig, logger *slog.Logger) *Handler {
	return &Handler{store: st, cache: c, cfg: cfg, logger: logger}
}

func normalizeSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if idx := strings.IndexByte(s, '.'); idx != -1 {
		return s[:idx]
	}
	return s
}

func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	signature := r.Header.Get("X-Signal-Signature")
	timestamp := r.Header.Get("X-Signal-Timestamp")
	if signature == "" || timestamp == "" {
		writeError(w, "missing signature headers", http.StatusUnauthorized)
		return
	}

	var sig InboundSignal
	if err := json.NewDecoder(r.Body).Decode(&sig); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if err := validate(sig); err != nil {
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Normalize before HMAC verification — must match what the EA signed
	sig.Symbol = normalizeSymbol(sig.Symbol)

	if err := auth.VerifySignalHMACWithTTL(
		h.cfg.SignalHMACSecret,
		strconv.FormatInt(sig.TicketID, 10),
		string(sig.SignalType),
		sig.Symbol,
		timestamp,
		signature,
		h.cfg.SignalTimestampTTL,
	); err != nil {
		h.logger.Warn("signal hmac verification failed",
			"err", err,
			"ip", r.RemoteAddr,
			"ticket_id", sig.TicketID,
		)
		writeError(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	isNew, err := h.cache.SetDedup(ctx, sig.TicketID, string(sig.SignalType), h.cfg.DeduplicateTTL)
	if err != nil {
		h.logger.Error("dedup check failed", "err", err)
	}
	if !isNew {
		h.logger.Info("duplicate signal ignored", "ticket_id", sig.TicketID, "type", sig.SignalType)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
		return
	}

	storeSig := &store.Signal{
		TicketID:   sig.TicketID,
		SignalType: string(sig.SignalType),
		Symbol:     sig.Symbol,
		Direction:  sig.Direction,
		Price:      sig.Price,
		SL:         sig.SL,
		TP:         sig.TP,
		Lot:        sig.Lot,
	}
	signalID, err := h.store.InsertSignal(ctx, storeSig)
	if err != nil {
		h.logger.Error("insert signal", "err", err)
		writeError(w, "storage error", http.StatusInternalServerError)
		return
	}

	if err := h.cache.SetLatestSignal(ctx, sig.Symbol, sig); err != nil {
		h.logger.Error("set latest signal cache", "err", err, "symbol", sig.Symbol)
	}

	job := Job{
		JobID:      uuid.New().String(),
		SignalID:   signalID,
		Signal:     sig,
		Attempts:   0,
		EnqueuedAt: time.Now(),
	}
	if err := h.cache.EnqueueJob(ctx, sig.Symbol, job); err != nil {
		h.logger.Error("enqueue job", "err", err, "signal_id", signalID)
	}

	h.logger.Info("signal accepted",
		"signal_id", signalID,
		"ticket_id", sig.TicketID,
		"type", sig.SignalType,
		"symbol", sig.Symbol,
		"direction", sig.Direction,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "signal_id": signalID})
}

// GetPending handles GET /pending/{symbol} — polled by receiver EAs every 300ms.
func (h *Handler) GetPending(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(r.PathValue("symbol"))
	if symbol == "" {
		writeError(w, "symbol required", http.StatusBadRequest)
		return
	}

	data, err := h.cache.GetLatestSignal(r.Context(), symbol)
	if err != nil {
		h.logger.Error("get latest signal", "err", err, "symbol", symbol)
		writeError(w, "cache error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if data == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func validate(sig InboundSignal) error {
	if sig.TicketID <= 0 {
		return fmt.Errorf("ticket_id required")
	}
	switch sig.SignalType {
	case SignalOpen, SignalModify, SignalClose, SignalPartial:
	default:
		return fmt.Errorf("invalid signal_type: %s", sig.SignalType)
	}
	if sig.Symbol == "" {
		return fmt.Errorf("symbol required")
	}
	switch sig.Direction {
	case "BUY", "SELL":
	default:
		return fmt.Errorf("direction must be BUY or SELL")
	}
	if sig.Price <= 0 {
		return fmt.Errorf("price must be > 0")
	}
	if sig.Lot <= 0 {
		return fmt.Errorf("lot must be > 0")
	}
	return nil
}

func writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
