package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mt4signal/internal/auth"
	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/store"
)

type Handler struct {
	store   *store.Store
	cache   *cache.Client
	authSvc *auth.Service
	logger  *slog.Logger
}

func New(st *store.Store, c *cache.Client, authSvc *auth.Service, logger *slog.Logger) *Handler {
	return &Handler{store: st, cache: c, authSvc: authSvc, logger: logger}
}

// ---- Auth ----

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	access, refresh, err := h.authSvc.AdminLogin(req.Username, req.Password)
	if err != nil {
		h.logger.Warn("admin login failed", "username", req.Username, "ip", r.RemoteAddr)
		writeError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	h.logger.Info("admin login", "username", req.Username)
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "Bearer",
	})
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	access, err := h.authSvc.RefreshAdminToken(r.Context(), req.RefreshToken)
	if err != nil {
		writeError(w, "invalid or expired refresh token", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": access, "token_type": "Bearer"})
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.RefreshToken != "" {
		h.authSvc.RevokeAdminToken(r.Context(), req.RefreshToken)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// ---- API Keys ----

func (h *Handler) IssueKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner       string   `json:"owner"`
		Scopes      []string `json:"scopes"`
		Note        string   `json:"note"`
		MaxMachines *int     `json:"max_machines"` // null = unlimited
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Owner == "" {
		writeError(w, "owner required", http.StatusBadRequest)
		return
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{auth.ScopeCopyReceive}
	}
	if req.MaxMachines != nil && *req.MaxMachines < 1 {
		writeError(w, "max_machines must be >= 1 or null for unlimited", http.StatusBadRequest)
		return
	}

	plaintext, hash, err := auth.GenerateKey()
	if err != nil {
		writeError(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	key, err := h.store.CreateAPIKey(r.Context(), hash, req.Owner, req.Note, req.Scopes, req.MaxMachines)
	if err != nil {
		h.logger.Error("create api key", "err", err)
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	h.logger.Info("api key issued", "owner", req.Owner, "key_id", key.ID, "max_machines", req.MaxMachines)
	writeJSON(w, http.StatusCreated, map[string]any{
		"key_id":       key.ID,
		"owner":        key.Owner,
		"scopes":       key.Scopes,
		"note":         key.Note,
		"max_machines": key.MaxMachines,
		"api_key":      plaintext,
		"created_at":   key.CreatedAt,
		"warning":      "store this key securely — it will not be shown again",
	})
}

func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListAPIKeys(r.Context())
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	type view struct {
		ID          uuid.UUID  `json:"id"`
		Owner       string     `json:"owner"`
		Scopes      []string   `json:"scopes"`
		Status      string     `json:"status"`
		Note        string     `json:"note"`
		MaxMachines *int       `json:"max_machines"`
		RotateAt    *time.Time `json:"rotate_at,omitempty"`
		CreatedAt   time.Time  `json:"created_at"`
		LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	}
	var out []view
	for _, k := range keys {
		out = append(out, view{
			ID: k.ID, Owner: k.Owner, Scopes: k.Scopes,
			Status: string(k.Status), Note: k.Note, MaxMachines: k.MaxMachines,
			RotateAt: k.RotateAt, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) SetKeyStatus(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid key id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	switch store.APIKeyStatus(req.Status) {
	case store.APIKeyActive, store.APIKeyRevoked, store.APIKeySuspended:
	default:
		writeError(w, "status must be active|revoked|suspended", http.StatusBadRequest)
		return
	}
	key, err := h.store.GetAPIKeyByID(r.Context(), id)
	if err != nil || key == nil {
		writeError(w, "key not found", http.StatusNotFound)
		return
	}
	if err := h.store.SetAPIKeyStatus(r.Context(), id, store.APIKeyStatus(req.Status)); err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	h.cache.DeleteAPIKeyCache(r.Context(), key.KeyHash)
	h.logger.Info("api key status changed", "key_id", id, "status", req.Status)
	writeJSON(w, http.StatusOK, map[string]string{"key_id": id.String(), "status": req.Status})
}

func (h *Handler) SetKeyMaxMachines(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid key id", http.StatusBadRequest)
		return
	}
	var req struct {
		MaxMachines *int `json:"max_machines"` // null = unlimited
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if req.MaxMachines != nil && *req.MaxMachines < 1 {
		writeError(w, "max_machines must be >= 1 or null for unlimited", http.StatusBadRequest)
		return
	}
	key, err := h.store.GetAPIKeyByID(r.Context(), id)
	if err != nil || key == nil {
		writeError(w, "key not found", http.StatusNotFound)
		return
	}
	if err := h.store.SetAPIKeyMaxMachines(r.Context(), id, req.MaxMachines); err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	// Invalidate cache so new limit takes effect immediately
	h.cache.DeleteAPIKeyCache(r.Context(), key.KeyHash)
	h.logger.Info("api key max_machines updated", "key_id", id, "max_machines", req.MaxMachines)
	writeJSON(w, http.StatusOK, map[string]any{"key_id": id.String(), "max_machines": req.MaxMachines})
}

func (h *Handler) RotateKey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid key id", http.StatusBadRequest)
		return
	}
	var req struct {
		RotateAfter string `json:"rotate_after"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.RotateAfter == "" {
		req.RotateAfter = "24h"
	}
	dur, err := time.ParseDuration(req.RotateAfter)
	if err != nil {
		writeError(w, "invalid rotate_after", http.StatusBadRequest)
		return
	}
	old, err := h.store.GetAPIKeyByID(r.Context(), id)
	if err != nil || old == nil {
		writeError(w, "key not found", http.StatusNotFound)
		return
	}
	plaintext, hash, err := auth.GenerateKey()
	if err != nil {
		writeError(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	rotateAt := time.Now().Add(dur)
	newKey, err := h.store.RotateAPIKey(r.Context(), id, hash, old.Owner, old.Note, old.Scopes, old.MaxMachines, rotateAt)
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	h.logger.Info("api key rotated", "old", id, "new", newKey.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"new_key_id": newKey.ID,
		"api_key":    plaintext,
		"rotate_at":  rotateAt,
		"message":    "old key remains valid until rotate_at",
	})
}

// ---- Machine Management ----

// GET /admin/machines  — all registrations across all keys
func (h *Handler) ListAllMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := h.store.ListMachines(r.Context(), nil)
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, machines)
}

// GET /admin/keys/{id}/machines  — registrations for one key
func (h *Handler) ListKeyMachines(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid key id", http.StatusBadRequest)
		return
	}
	machines, err := h.store.ListMachines(r.Context(), &id)
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	count, _ := h.store.CountMachines(r.Context(), id)
	key, _ := h.store.GetAPIKeyByID(r.Context(), id)
	resp := map[string]any{
		"machines":     machines,
		"count":        count,
		"max_machines": nil,
	}
	if key != nil {
		resp["max_machines"] = key.MaxMachines
	}
	writeJSON(w, http.StatusOK, resp)
}

// DELETE /admin/keys/{id}/machines/{account}  — remove a registration (lets that slot be re-used)
func (h *Handler) RemoveMachine(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid key id", http.StatusBadRequest)
		return
	}
	account := r.PathValue("account")
	if account == "" {
		writeError(w, "account number required", http.StatusBadRequest)
		return
	}
	if err := h.store.RemoveMachine(r.Context(), id, account); err != nil {
		writeError(w, err.Error(), http.StatusNotFound)
		return
	}
	h.logger.Info("machine removed", "key_id", id, "account", account)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "account": account})
}

// ListMachines is the route-facing alias for ListKeyMachines.
func (h *Handler) ListMachines(w http.ResponseWriter, r *http.Request) {
	h.ListKeyMachines(w, r)
}

// SetMaxMachines PATCH /admin/keys/{id}/machines — update the machine limit for a key.
func (h *Handler) SetMaxMachines(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid key id", http.StatusBadRequest)
		return
	}
	var req struct {
		MaxMachines *int `json:"max_machines"` // null = unlimited
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if req.MaxMachines != nil && *req.MaxMachines < 0 {
		writeError(w, "max_machines must be >= 0 or null (unlimited)", http.StatusBadRequest)
		return
	}
	if err := h.store.SetAPIKeyMaxMachines(r.Context(), id, req.MaxMachines); err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	h.cache.InvalidateMachineCache(r.Context(), id.String())
	h.logger.Info("max machines updated", "key_id", id, "max", req.MaxMachines)
	writeJSON(w, http.StatusOK, map[string]any{"key_id": id, "max_machines": req.MaxMachines})
}

// ---- Subscribers ----

func (h *Handler) CreateSubscriber(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KeyID   string         `json:"key_id"`
		Channel string         `json:"channel"`
		Config  map[string]any `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	keyID, err := uuid.Parse(req.KeyID)
	if err != nil {
		writeError(w, "invalid key_id", http.StatusBadRequest)
		return
	}
	sub, err := h.store.CreateSubscriber(r.Context(), keyID, req.Channel, req.Config)
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, sub)
}

func (h *Handler) ListSubscribers(w http.ResponseWriter, r *http.Request) {
	subs, err := h.store.ListAllSubscribers(r.Context())
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, subs)
}

func (h *Handler) SetSubscriberActive(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, "invalid id", http.StatusBadRequest)
		return
	}
	var req struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if err := h.store.SetSubscriberActive(r.Context(), id, req.Active); err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": req.Active})
}

// ---- Symbols ----

func (h *Handler) ListSymbols(w http.ResponseWriter, r *http.Request) {
	symbols, err := h.store.ListAllSymbols(r.Context())
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, symbols)
}

func (h *Handler) AddSymbol(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Symbol string `json:"symbol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	req.Symbol = strings.ToUpper(strings.TrimSpace(req.Symbol))
	if req.Symbol == "" {
		writeError(w, "symbol required", http.StatusBadRequest)
		return
	}
	if err := h.store.AddSymbol(r.Context(), req.Symbol); err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	// Invalidate cache so worker picks it up within TTL
	h.cache.InvalidateSymbolList(r.Context())
	h.logger.Info("symbol added", "symbol", req.Symbol)
	writeJSON(w, http.StatusCreated, map[string]string{"symbol": req.Symbol, "status": "added"})
}

func (h *Handler) SetSymbolActive(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(r.PathValue("symbol"))
	if symbol == "" {
		writeError(w, "symbol required", http.StatusBadRequest)
		return
	}
	var req struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if err := h.store.SetSymbolActive(r.Context(), symbol, req.Active); err != nil {
		writeError(w, err.Error(), http.StatusNotFound)
		return
	}
	h.cache.InvalidateSymbolList(r.Context())
	h.logger.Info("symbol active state changed", "symbol", symbol, "active", req.Active)
	writeJSON(w, http.StatusOK, map[string]any{"symbol": symbol, "active": req.Active})
}

func (h *Handler) DeleteSymbol(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(r.PathValue("symbol"))
	if symbol == "" {
		writeError(w, "symbol required", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteSymbol(r.Context(), symbol); err != nil {
		writeError(w, err.Error(), http.StatusNotFound)
		return
	}
	h.cache.InvalidateSymbolList(r.Context())
	h.logger.Info("symbol deleted", "symbol", symbol)
	writeJSON(w, http.StatusOK, map[string]string{"symbol": symbol, "status": "deleted"})
}

// ---- Metrics + Signals ----

func (h *Handler) GetMetrics(w http.ResponseWriter, r *http.Request) {
	metrics, err := h.store.GetMetrics(r.Context())
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	deadLetterDepth, _ := h.cache.DeadLetterDepth(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"total_signals":      metrics.TotalSignals,
		"delivered_count":    metrics.DeliveredCount,
		"failed_count":       metrics.FailedCount,
		"dead_count":         metrics.DeadCount,
		"active_subscribers": metrics.ActiveSubscribers,
		"total_api_keys":     metrics.TotalAPIKeys,
		"active_api_keys":    metrics.ActiveAPIKeys,
		"dead_letter_queue":  deadLetterDepth,
	})
}

func (h *Handler) ListSignals(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sigs, err := h.store.ListSignals(r.Context(), limit, offset)
	if err != nil {
		writeError(w, "store error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, sigs)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, msg string, code int) {
	writeJSON(w, code, map[string]string{"error": msg})
}
