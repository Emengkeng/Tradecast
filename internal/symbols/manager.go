package symbols

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/queue"
	"github.com/mt4signal/internal/store"
)

const cacheTTL = 5 * time.Minute

// Manager loads the watched symbol list from cache/DB and keeps the
// worker consumer in sync. Every TTL it re-checks for additions or
// removals, starting/stopping per-symbol goroutines accordingly.
type Manager struct {
	store    *store.Store
	cache    *cache.Client
	consumer *queue.Consumer
	logger   *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func NewManager(st *store.Store, c *cache.Client, consumer *queue.Consumer, logger *slog.Logger) *Manager {
	return &Manager{
		store:    st,
		cache:    c,
		consumer: consumer,
		logger:   logger,
		running:  make(map[string]context.CancelFunc),
	}
}

// Start loads symbols and begins watching for changes every cacheTTL.
func (m *Manager) Start(ctx context.Context) {
	symbols, err := m.load(ctx)
	if err != nil {
		m.logger.Error("symbol manager: initial load failed", "err", err)
	}

	for _, sym := range symbols {
		m.startSymbol(ctx, sym)
	}
	m.logger.Info("symbol manager started", "count", len(symbols), "symbols", symbols)

	ticker := time.NewTicker(cacheTTL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("symbol manager stopping")
			return
		case <-ticker.C:
			m.sync(ctx)
		}
	}
}

// sync diffs running vs desired and starts/stops accordingly.
func (m *Manager) sync(ctx context.Context) {
	symbols, err := m.load(ctx)
	if err != nil {
		m.logger.Error("symbol manager sync failed", "err", err)
		return
	}

	desired := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		desired[s] = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for sym := range desired {
		if _, ok := m.running[sym]; !ok {
			m.startLocked(ctx, sym)
			m.logger.Info("symbol manager: added", "symbol", sym)
		}
	}

	for sym, cancel := range m.running {
		if !desired[sym] {
			cancel()
			delete(m.running, sym)
			m.logger.Info("symbol manager: removed", "symbol", sym)
		}
	}
}

func (m *Manager) startSymbol(ctx context.Context, sym string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startLocked(ctx, sym)
}

func (m *Manager) startLocked(ctx context.Context, sym string) {
	if _, exists := m.running[sym]; exists {
		return
	}
	symCtx, cancel := context.WithCancel(ctx)
	m.running[sym] = cancel
	m.consumer.AddSymbol(sym)
	go m.consumer.ConsumeOne(symCtx, sym)
}

// load fetches from cache, falls back to DB (which falls back to hardcoded defaults).
func (m *Manager) load(ctx context.Context) ([]string, error) {
	cached, err := m.cache.GetSymbolList(ctx)
	if err != nil {
		m.logger.Warn("symbol cache read error", "err", err)
	}
	if len(cached) > 0 {
		return cached, nil
	}

	syms, err := m.store.ListActiveSymbols(ctx)
	if err != nil {
		return nil, err
	}

	if err := m.cache.SetSymbolList(ctx, syms, cacheTTL); err != nil {
		m.logger.Warn("symbol cache write error", "err", err)
	}
	return syms, nil
}

// NewLoader is an alias for NewManager for use in cmd/worker.
func NewLoader(st *store.Store, c *cache.Client, consumer *queue.Consumer, logger *slog.Logger) *Manager {
	return NewManager(st, c, consumer, logger)
}
