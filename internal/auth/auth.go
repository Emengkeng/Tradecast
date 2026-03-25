package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/config"
	"github.com/mt4signal/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	ScopeCopyReceive     = "copy:receive"
	ScopeSignalsRead     = "signals:read"
	ScopeWebhookRegister = "webhook:register"
)

// ---- Key generation ----

func GenerateKey() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	plaintext = hex.EncodeToString(b)
	hash = HashKey(plaintext)
	return
}

func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ---- Signal HMAC verification ----
// MT4 signs: HMAC-SHA256(secret, "ticket_id:signal_type:SYMBOL:timestamp")
// Timestamp must be RFC3339. Requests older than TTL are rejected (replay protection).

func VerifySignalHMACWithTTL(secret, ticketID, signalType, symbol, timestamp, signature string, ttl time.Duration) error {
	ts, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return fmt.Errorf("invalid timestamp format")
	}
	age := time.Since(ts)
	if age < 0 {
		age = -age
	}
	if age > ttl {
		return fmt.Errorf("signal timestamp too old (%v) — possible replay attack", age)
	}
	message := fmt.Sprintf("%s:%s:%s:%s", ticketID, signalType, symbol, timestamp)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return fmt.Errorf("invalid signal signature")
	}
	return nil
}

func GenerateSignalHMAC(secret, ticketID, signalType, symbol, timestamp string) string {
	message := fmt.Sprintf("%s:%s:%s:%s", ticketID, signalType, symbol, timestamp)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---- API key cache type ----

type CachedKey struct {
	ID     string   `json:"id"`
	Owner  string   `json:"owner"`
	Scopes []string `json:"scopes"`
	Status string   `json:"status"`
}

// ---- Service ----

type Service struct {
	store  *store.Store
	cache  *cache.Client
	cfg    config.AuthConfig
	logger *slog.Logger
}

func NewService(st *store.Store, c *cache.Client, cfg config.AuthConfig, logger *slog.Logger) *Service {
	return &Service{store: st, cache: c, cfg: cfg, logger: logger}
}

// ValidateKey checks cache first, then DB. Returns nil if key is invalid/revoked.
func (s *Service) ValidateKey(ctx context.Context, plaintext string) (*CachedKey, error) {
	hash := HashKey(plaintext)

	cached, err := s.cache.GetAPIKeyCache(ctx, hash)
	if err != nil {
		s.logger.Warn("api key cache get", "err", err)
	}
	if cached != nil {
		var ck CachedKey
		if err := json.Unmarshal(cached, &ck); err == nil {
			if ck.Status == string(store.APIKeyActive) {
				return &ck, nil
			}
			return nil, nil
		}
	}

	key, err := s.store.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("get api key: %w", err)
	}
	if key == nil || key.Status != store.APIKeyActive {
		return nil, nil
	}
	if key.RotateAt != nil && time.Now().After(*key.RotateAt) {
		return nil, nil
	}

	ck := &CachedKey{ID: key.ID.String(), Owner: key.Owner, Scopes: key.Scopes, Status: string(key.Status)}
	b, _ := json.Marshal(ck)
	s.cache.SetAPIKeyCache(ctx, hash, b, s.cfg.KeyCacheTTL)
	go s.store.TouchAPIKeyLastUsed(context.Background(), key.ID)
	return ck, nil
}

func (s *Service) HasScope(key *CachedKey, scope string) bool {
	for _, sc := range key.Scopes {
		if sc == scope {
			return true
		}
	}
	return false
}

// ---- Machine binding ----

// CheckMachineBinding verifies the MT4 account number against the key's registered machines.
// Auto-registers if slots are available. Hard rejects if limit reached.
func (s *Service) CheckMachineBinding(ctx context.Context, ck *CachedKey, accountNumber string) error {
	keyID, err := uuid.Parse(ck.ID)
	if err != nil {
		return fmt.Errorf("invalid key id")
	}

	// No account number sent — only allow if this key has zero registered machines
	if accountNumber == "" {
		count, err := s.store.CountMachines(ctx, keyID)
		if err != nil {
			s.logger.Warn("count machines", "err", err)
			return nil // fail open on DB error
		}
		if count > 0 {
			return fmt.Errorf("X-MT4-Account header required for this key")
		}
		return nil
	}

	// Fast path: check cache
	allowed, found, err := s.cache.GetMachineAllowed(ctx, ck.ID, accountNumber)
	if err != nil {
		s.logger.Warn("machine cache get", "err", err)
	}
	if found {
		if allowed {
			go s.store.TouchMachineLastSeen(context.Background(), keyID, accountNumber)
			return nil
		}
		return fmt.Errorf("machine not authorised: account %s is not registered on this key", accountNumber)
	}

	// Cache miss — check DB
	registered, err := s.store.IsMachineRegistered(ctx, keyID, accountNumber)
	if err != nil {
		s.logger.Warn("is machine registered", "err", err)
		return nil // fail open
	}
	if registered {
		s.cache.SetMachineAllowed(ctx, ck.ID, accountNumber, true, cache.MachineCacheTTL)
		go s.store.TouchMachineLastSeen(context.Background(), keyID, accountNumber)
		return nil
	}

	// Not registered — attempt auto-registration (RegisterMachine enforces max_machines)
	maxMachines, err := s.store.GetMaxMachines(ctx, keyID)
	if err != nil {
		s.logger.Warn("get max machines", "err", err)
		maxMachines = nil // treat as unlimited on error
	}
	if _, err := s.store.RegisterMachine(ctx, keyID, accountNumber, maxMachines); err != nil {
		if err == store.ErrMachineLimit {
			s.cache.SetMachineAllowed(ctx, ck.ID, accountNumber, false, cache.MachineCacheTTL)
			limit := 0
			if maxMachines != nil {
				limit = *maxMachines
			}
			return fmt.Errorf("machine limit reached: this key allows %d machine(s), all slots taken", limit)
		}
		s.logger.Error("register machine", "err", err)
		return nil // fail open
	}
	s.cache.SetMachineAllowed(ctx, ck.ID, accountNumber, true, cache.MachineCacheTTL)
	s.logger.Info("machine auto-registered", "key_id", ck.ID, "account", accountNumber, "owner", ck.Owner)
	return nil
}

// ---- JWT admin auth ----

type AdminClaims struct {
	Username string `json:"username"`
	JTI      string `json:"jti"`
	jwt.RegisteredClaims
}

func (s *Service) AdminLogin(username, password string) (accessToken, refreshToken string, err error) {
	if username != s.cfg.AdminUsername {
		return "", "", fmt.Errorf("invalid credentials")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.AdminPasswordHash), []byte(password)); err != nil {
		return "", "", fmt.Errorf("invalid credentials")
	}
	access, err := s.makeToken(username, s.cfg.JWTAccessTokenTTL, "access")
	if err != nil {
		return "", "", err
	}
	refresh, err := s.makeToken(username, s.cfg.JWTRefreshTokenTTL, "refresh")
	return access, refresh, err
}

func (s *Service) makeToken(username string, ttl time.Duration, tokenType string) (string, error) {
	claims := AdminClaims{
		Username: username,
		JTI:      uuid.New().String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			Issuer:    "mt4signal",
			Audience:  jwt.ClaimStrings{"mt4signal-admin-" + tokenType},
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.cfg.JWTSecret))
}

func (s *Service) ValidateAdminToken(ctx context.Context, tokenStr string) (*AdminClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &AdminClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(s.cfg.JWTSecret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(*AdminClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	blocked, err := s.cache.IsJWTBlocked(ctx, claims.JTI)
	if err != nil {
		s.logger.Warn("jwt blocklist check", "err", err)
	}
	if blocked {
		return nil, fmt.Errorf("token revoked")
	}
	return claims, nil
}

func (s *Service) RefreshAdminToken(ctx context.Context, refreshTokenStr string) (string, error) {
	claims, err := s.ValidateAdminToken(ctx, refreshTokenStr)
	if err != nil {
		return "", err
	}
	remaining := time.Until(claims.ExpiresAt.Time)
	if remaining > 0 {
		s.cache.BlockJWT(ctx, claims.JTI, remaining)
	}
	return s.makeToken(claims.Username, s.cfg.JWTAccessTokenTTL, "access")
}

func (s *Service) RevokeAdminToken(ctx context.Context, tokenStr string) error {
	claims, err := s.ValidateAdminToken(ctx, tokenStr)
	if err != nil {
		return err
	}
	remaining := time.Until(claims.ExpiresAt.Time)
	if remaining > 0 {
		return s.cache.BlockJWT(ctx, claims.JTI, remaining)
	}
	return nil
}

// ---- HTTP middleware ----

type contextKey string

const (
	ContextKeyAPIKey    contextKey = "api_key"
	ContextKeyAdminUser contextKey = "admin_user"
)

func (s *Service) APIKeyMiddleware(requiredScope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := realIP(r)

			failures, _ := s.cache.GetAuthFailures(r.Context(), ip)
			if failures >= int64(s.cfg.RateLimitMaxFail) {
				writeJSONError(w, "too many auth failures, try later", http.StatusTooManyRequests)
				return
			}

			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				s.recordFailure(r.Context(), ip)
				writeJSONError(w, "missing X-API-Key header", http.StatusUnauthorized)
				return
			}

			ck, err := s.ValidateKey(r.Context(), apiKey)
			if err != nil {
				s.logger.Error("validate key", "err", err)
				writeJSONError(w, "internal error", http.StatusInternalServerError)
				return
			}
			if ck == nil {
				s.recordFailure(r.Context(), ip)
				writeJSONError(w, "invalid or revoked api key", http.StatusUnauthorized)
				return
			}
			if requiredScope != "" && !s.HasScope(ck, requiredScope) {
				writeJSONError(w, "insufficient scope", http.StatusForbidden)
				return
			}

			// Machine binding — check X-Account-ID header
			accountNumber := strings.TrimSpace(r.Header.Get("X-Account-ID"))
			if err := s.CheckMachineBinding(r.Context(), ck, accountNumber); err != nil {
				s.logger.Warn("machine binding rejected",
					"key_id", ck.ID, "account", accountNumber, "err", err)
				writeJSONError(w, err.Error(), http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), ContextKeyAPIKey, ck)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (s *Service) JWTMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := extractBearer(r)
		if tokenStr == "" {
			writeJSONError(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}
		claims, err := s.ValidateAdminToken(r.Context(), tokenStr)
		if err != nil {
			writeJSONError(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ContextKeyAdminUser, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) recordFailure(ctx context.Context, ip string) {
	if _, err := s.cache.IncrAuthFailure(ctx, ip, s.cfg.RateLimitWindow); err != nil {
		s.logger.Warn("incr auth failure", "err", err)
	}
}

func APIKeyFromContext(ctx context.Context) *CachedKey {
	v, _ := ctx.Value(ContextKeyAPIKey).(*CachedKey)
	return v
}

func AdminUserFromContext(ctx context.Context) *AdminClaims {
	v, _ := ctx.Value(ContextKeyAdminUser).(*AdminClaims)
	return v
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return h[7:]
	}
	return ""
}

func realIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0])
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return rip
	}
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
