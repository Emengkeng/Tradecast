package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

type Store struct{ db *sql.DB }

func New(dsn string, maxOpen, maxIdle int) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// ---- API Keys ----

type APIKeyStatus string

const (
	APIKeyActive    APIKeyStatus = "active"
	APIKeyRevoked   APIKeyStatus = "revoked"
	APIKeySuspended APIKeyStatus = "suspended"
)

type APIKey struct {
	ID             uuid.UUID
	KeyHash        string
	Owner          string
	Scopes         []string
	Status         APIKeyStatus
	MaxMachines    *int // nil = unlimited
	SuccessorKeyID *uuid.UUID
	RotateAt       *time.Time
	Note           string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

func (s *Store) CreateAPIKey(ctx context.Context, keyHash, owner, note string, scopes []string, maxMachines *int) (*APIKey, error) {
	id := uuid.New()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, key_hash, owner, scopes, note, status, max_machines)
		 VALUES ($1,$2,$3,$4,$5,'active',$6)`,
		id, keyHash, owner, scopesToArray(scopes), note, maxMachines,
	)
	if err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	return s.GetAPIKeyByID(ctx, id)
}

func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id,key_hash,owner,scopes,status,max_machines,successor_key_id,rotate_at,note,created_at,last_used_at
		 FROM api_keys WHERE key_hash=$1`, keyHash)
	return scanAPIKey(row)
}

func (s *Store) GetAPIKeyByID(ctx context.Context, id uuid.UUID) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id,key_hash,owner,scopes,status,max_machines,successor_key_id,rotate_at,note,created_at,last_used_at
		 FROM api_keys WHERE id=$1`, id)
	return scanAPIKey(row)
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,key_hash,owner,scopes,status,max_machines,successor_key_id,rotate_at,note,created_at,last_used_at
		 FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) SetAPIKeyStatus(ctx context.Context, id uuid.UUID, status APIKeyStatus) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET status=$1 WHERE id=$2`, status, id)
	return err
}

func (s *Store) SetAPIKeyMaxMachines(ctx context.Context, id uuid.UUID, maxMachines *int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET max_machines=$1 WHERE id=$2`, maxMachines, id)
	return err
}

func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, id uuid.UUID) {
	s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=NOW() WHERE id=$1`, id)
}

func (s *Store) RotateAPIKey(ctx context.Context, oldID uuid.UUID, newKeyHash, owner, note string, scopes []string, maxMachines *int, rotateAt time.Time) (*APIKey, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	newID := uuid.New()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO api_keys (id,key_hash,owner,scopes,note,status,max_machines) VALUES ($1,$2,$3,$4,$5,'active',$6)`,
		newID, newKeyHash, owner, scopesToArray(scopes), note, maxMachines); err != nil {
		return nil, fmt.Errorf("create successor: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE api_keys SET successor_key_id=$1, rotate_at=$2 WHERE id=$3`,
		newID, rotateAt, oldID); err != nil {
		return nil, fmt.Errorf("link successor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAPIKeyByID(ctx, newID)
}

// ---- Machine Binding ----

type KeyMachine struct {
	ID            uuid.UUID
	KeyID         uuid.UUID
	AccountNumber string
	RegisteredAt  time.Time
	LastSeenAt    time.Time
}

// RegisterMachine attempts to register an account number to a key.
// Returns (true, nil) if newly registered.
// Returns (false, nil) if already registered (existing machine, allow through).
// Returns (false, ErrMachineLimit) if max_machines reached.
var ErrMachineLimit = fmt.Errorf("machine limit reached for this api key")

func (s *Store) RegisterMachine(ctx context.Context, keyID uuid.UUID, accountNumber string, maxMachines *int) (bool, error) {
	// Check if already registered — if so, touch last_seen and allow
	var existing uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM key_machines WHERE key_id=$1 AND account_number=$2`,
		keyID, accountNumber).Scan(&existing)

	if err == nil {
		// Already registered — update last_seen async
		go s.db.ExecContext(context.Background(),
			`UPDATE key_machines SET last_seen_at=NOW() WHERE id=$1`, existing)
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("check machine: %w", err)
	}

	// Not registered — check limit
	if maxMachines != nil {
		var count int
		s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM key_machines WHERE key_id=$1`, keyID).Scan(&count)
		if count >= *maxMachines {
			return false, ErrMachineLimit
		}
	}

	// Register
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO key_machines (id,key_id,account_number) VALUES ($1,$2,$3)`,
		uuid.New(), keyID, accountNumber)
	if err != nil {
		// Race condition: another request registered concurrently
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			return false, nil
		}
		return false, fmt.Errorf("register machine: %w", err)
	}
	return true, nil
}

func (s *Store) ListMachines(ctx context.Context, keyID *uuid.UUID) ([]*KeyMachine, error) {
	query := `SELECT id,key_id,account_number,registered_at,last_seen_at FROM key_machines`
	args := []any{}
	if keyID != nil {
		query += ` WHERE key_id=$1`
		args = append(args, *keyID)
	}
	query += ` ORDER BY registered_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var machines []*KeyMachine
	for rows.Next() {
		m := &KeyMachine{}
		if err := rows.Scan(&m.ID, &m.KeyID, &m.AccountNumber, &m.RegisteredAt, &m.LastSeenAt); err != nil {
			return nil, err
		}
		machines = append(machines, m)
	}
	return machines, rows.Err()
}

func (s *Store) RemoveMachine(ctx context.Context, keyID uuid.UUID, accountNumber string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM key_machines WHERE key_id=$1 AND account_number=$2`, keyID, accountNumber)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("machine not found")
	}
	return nil
}

func (s *Store) CountMachines(ctx context.Context, keyID uuid.UUID) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM key_machines WHERE key_id=$1`, keyID).Scan(&n)
	return n, err
}

// ---- Watched Symbols ----

var defaultSymbols = []string{
	"EURUSD", "GBPUSD", "USDJPY", "XAUUSD",
	"USDCHF", "AUDUSD", "USDCAD", "NZDUSD",
	"GBPJPY", "EURJPY", "XAGUSD", "BTCUSD",
}

type WatchedSymbol struct {
	ID        uuid.UUID
	Symbol    string
	Active    bool
	CreatedAt time.Time
}

func (s *Store) ListActiveSymbols(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT symbol FROM watched_symbols WHERE active=TRUE ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var syms []string
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fall back to hardcoded defaults if DB is empty
	if len(syms) == 0 {
		return defaultSymbols, nil
	}
	return syms, nil
}

func (s *Store) ListAllSymbols(ctx context.Context) ([]*WatchedSymbol, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,symbol,active,created_at FROM watched_symbols ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WatchedSymbol
	for rows.Next() {
		w := &WatchedSymbol{}
		if err := rows.Scan(&w.ID, &w.Symbol, &w.Active, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) AddSymbol(ctx context.Context, symbol string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO watched_symbols (id,symbol,active) VALUES ($1,$2,TRUE)
		 ON CONFLICT (symbol) DO UPDATE SET active=TRUE`,
		uuid.New(), strings.ToUpper(strings.TrimSpace(symbol)))
	return err
}

func (s *Store) SetSymbolActive(ctx context.Context, symbol string, active bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE watched_symbols SET active=$1 WHERE symbol=$2`, active, symbol)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("symbol not found: %s", symbol)
	}
	return nil
}

func (s *Store) DeleteSymbol(ctx context.Context, symbol string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM watched_symbols WHERE symbol=$1`, symbol)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("symbol not found: %s", symbol)
	}
	return nil
}

// ---- Signals ----

type Signal struct {
	ID          uuid.UUID
	TicketID    int64
	SignalType  string
	Symbol      string
	Direction   string
	Price       float64
	SL          *float64
	TP          *float64
	Lot         float64
	SourceKeyID *uuid.UUID
	ReceivedAt  time.Time
}

func (s *Store) InsertSignal(ctx context.Context, sig *Signal) (uuid.UUID, error) {
	id := uuid.New()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO signals (id,ticket_id,signal_type,symbol,direction,price,sl,tp,lot,source_key_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, sig.TicketID, sig.SignalType, sig.Symbol, sig.Direction,
		sig.Price, sig.SL, sig.TP, sig.Lot, sig.SourceKeyID,
	)
	return id, err
}

func (s *Store) ListSignals(ctx context.Context, limit, offset int) ([]*Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,ticket_id,signal_type,symbol,direction,price,sl,tp,lot,source_key_id,received_at
		 FROM signals ORDER BY received_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sigs []*Signal
	for rows.Next() {
		sig := &Signal{}
		if err := rows.Scan(&sig.ID, &sig.TicketID, &sig.SignalType, &sig.Symbol, &sig.Direction,
			&sig.Price, &sig.SL, &sig.TP, &sig.Lot, &sig.SourceKeyID, &sig.ReceivedAt); err != nil {
			return nil, err
		}
		sigs = append(sigs, sig)
	}
	return sigs, rows.Err()
}

// ---- Subscribers ----

type Subscriber struct {
	ID        uuid.UUID
	KeyID     uuid.UUID
	Channel   string
	Config    map[string]any
	Active    bool
	CreatedAt time.Time
}

func (s *Store) CreateSubscriber(ctx context.Context, keyID uuid.UUID, channel string, config map[string]any) (*Subscriber, error) {
	id := uuid.New()
	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO subscribers (id,key_id,channel,config) VALUES ($1,$2,$3,$4)`,
		id, keyID, channel, cfgJSON)
	if err != nil {
		return nil, err
	}
	return s.GetSubscriberByID(ctx, id)
}

func (s *Store) GetSubscriberByID(ctx context.Context, id uuid.UUID) (*Subscriber, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id,key_id,channel,config,active,created_at FROM subscribers WHERE id=$1`, id)
	return scanSubscriber(row)
}

func (s *Store) ListActiveSubscribers(ctx context.Context) ([]*Subscriber, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,key_id,channel,config,active,created_at FROM subscribers WHERE active=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSubscribers(rows)
}

func (s *Store) ListAllSubscribers(ctx context.Context) ([]*Subscriber, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,key_id,channel,config,active,created_at FROM subscribers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSubscribers(rows)
}

func (s *Store) SetSubscriberActive(ctx context.Context, id uuid.UUID, active bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subscribers SET active=$1 WHERE id=$2`, active, id)
	return err
}

// ---- Delivery Log ----

func (s *Store) InsertDeliveryLog(ctx context.Context, signalID, subscriberID uuid.UUID, channel string) (uuid.UUID, error) {
	id := uuid.New()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO delivery_log (id,signal_id,subscriber_id,channel,status) VALUES ($1,$2,$3,$4,'pending')`,
		id, signalID, subscriberID, channel)
	return id, err
}

func (s *Store) MarkDelivered(ctx context.Context, logID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delivery_log SET status='delivered',last_attempted_at=NOW(),attempts=attempts+1 WHERE id=$1`, logID)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, logID uuid.UUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delivery_log SET status='failed',last_attempted_at=NOW(),attempts=attempts+1,error=$1 WHERE id=$2`,
		errMsg, logID)
	return err
}

func (s *Store) MarkDead(ctx context.Context, logID uuid.UUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE delivery_log SET status='dead',last_attempted_at=NOW(),error=$1 WHERE id=$2`,
		errMsg, logID)
	return err
}

func (s *Store) InsertDeadLetter(ctx context.Context, jobPayload []byte, signalID *uuid.UUID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dead_letter (id,job_payload,signal_id,failure_reason) VALUES ($1,$2,$3,$4)`,
		uuid.New(), jobPayload, signalID, reason)
	return err
}

// ---- Metrics ----

type Metrics struct {
	TotalSignals      int64
	DeliveredCount    int64
	FailedCount       int64
	DeadCount         int64
	ActiveSubscribers int64
	TotalAPIKeys      int64
	ActiveAPIKeys     int64
}

func (s *Store) GetMetrics(ctx context.Context) (*Metrics, error) {
	m := &Metrics{}
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signals`).Scan(&m.TotalSignals)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_log WHERE status='delivered'`).Scan(&m.DeliveredCount)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_log WHERE status='failed'`).Scan(&m.FailedCount)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM delivery_log WHERE status='dead'`).Scan(&m.DeadCount)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscribers WHERE active=TRUE`).Scan(&m.ActiveSubscribers)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&m.TotalAPIKeys)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE status='active'`).Scan(&m.ActiveAPIKeys)
	return m, nil
}

// ---- Helpers ----

type scanner interface{ Scan(dest ...any) error }

func scanAPIKey(s scanner) (*APIKey, error) {
	k := &APIKey{}
	var scopes string
	err := s.Scan(&k.ID, &k.KeyHash, &k.Owner, &scopes, &k.Status, &k.MaxMachines,
		&k.SuccessorKeyID, &k.RotateAt, &k.Note, &k.CreatedAt, &k.LastUsedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Scopes = parsePostgresArray(scopes)
	return k, nil
}

func scanSubscriber(s scanner) (*Subscriber, error) {
	sub := &Subscriber{}
	var cfgJSON []byte
	err := s.Scan(&sub.ID, &sub.KeyID, &sub.Channel, &cfgJSON, &sub.Active, &sub.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(cfgJSON) > 0 {
		json.Unmarshal(cfgJSON, &sub.Config)
	}
	return sub, nil
}

func collectSubscribers(rows *sql.Rows) ([]*Subscriber, error) {
	var subs []*Subscriber
	for rows.Next() {
		sub, err := scanSubscriber(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func scopesToArray(scopes []string) string {
	if len(scopes) == 0 {
		return "{}"
	}
	return "{" + strings.Join(scopes, ",") + "}"
}

func parsePostgresArray(s string) []string {
	s = strings.Trim(s, "{}")
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsMachineRegistered is a simple lookup used by the auth layer.
func (s *Store) IsMachineRegistered(ctx context.Context, keyID uuid.UUID, accountNumber string) (bool, error) {
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM key_machines WHERE key_id=$1 AND account_number=$2`,
		keyID, accountNumber).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// GetMaxMachines returns the machine limit for a key. nil = unlimited.
func (s *Store) GetMaxMachines(ctx context.Context, keyID uuid.UUID) (*int, error) {
	var max *int
	err := s.db.QueryRowContext(ctx,
		`SELECT max_machines FROM api_keys WHERE id=$1`, keyID).Scan(&max)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return max, err
}

// TouchMachineLastSeen updates last_seen_at for a registered machine.
func (s *Store) TouchMachineLastSeen(ctx context.Context, keyID uuid.UUID, accountNumber string) {
	s.db.ExecContext(ctx,
		`UPDATE key_machines SET last_seen_at=NOW() WHERE key_id=$1 AND account_number=$2`,
		keyID, accountNumber)
}
