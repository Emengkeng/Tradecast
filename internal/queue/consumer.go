package queue

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/config"
	"github.com/mt4signal/internal/signal"
	"github.com/mt4signal/internal/store"
)

type Handler func(ctx context.Context, job *signal.Job) error

type Consumer struct {
	cache    *cache.Client
	store    *store.Store
	cfg      config.WorkerConfig
	logger   *slog.Logger
	handlers []Handler
	symbols  []string
	mu       sync.RWMutex
}

func NewConsumer(c *cache.Client, st *store.Store, cfg config.WorkerConfig, logger *slog.Logger) *Consumer {
	return &Consumer{cache: c, store: st, cfg: cfg, logger: logger}
}

func (c *Consumer) RegisterHandler(h Handler) {
	c.handlers = append(c.handlers, h)
}

func (c *Consumer) AddSymbol(sym string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.symbols {
		if s == sym {
			return
		}
	}
	c.symbols = append(c.symbols, sym)
}

// Symbols returns a copy of the currently registered symbol list.
func (c *Consumer) Symbols() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.symbols))
	copy(out, c.symbols)
	return out
}

// Start runs the stale-job cleanup loop.
// Per-symbol consume goroutines are managed by the symbols.Manager.
func (c *Consumer) Start(ctx context.Context) {
	c.runCleanup(ctx)
}

func (c *Consumer) consumeSymbol(ctx context.Context, symbol string) {
	c.logger.Info("consumer started", "symbol", symbol)
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("consumer stopped", "symbol", symbol)
			return
		default:
		}

		raw, err := c.cache.DequeueJob(ctx, symbol, 2*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Error("dequeue error", "symbol", symbol, "err", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if raw == nil {
			continue
		}

		var job signal.Job
		if err := json.Unmarshal(raw, &job); err != nil {
			c.logger.Error("unmarshal job error", "err", err)
			c.cache.AckJob(ctx, symbol, raw) // discard malformed
			continue
		}

		c.processJob(ctx, symbol, raw, &job)
	}
}

func (c *Consumer) processJob(ctx context.Context, symbol string, raw []byte, job *signal.Job) {
	var lastErr error
	for _, h := range c.handlers {
		if err := h(ctx, job); err != nil {
			lastErr = err
			c.logger.Error("handler error", "job_id", job.JobID, "symbol", symbol, "err", err)
		}
	}

	if lastErr == nil {
		c.cache.AckJob(ctx, symbol, raw)
		return
	}

	job.Attempts++

	if job.Attempts >= c.cfg.MaxRetries {
		c.logger.Error("job dead", "job_id", job.JobID, "attempts", job.Attempts)
		c.cache.MoveToDeadLetter(ctx, symbol, raw)
		c.store.InsertDeadLetter(ctx, raw, &job.SignalID, lastErr.Error())
		return
	}

	// Exponential backoff with jitter — prevents retry storms
	backoff := c.backoff(job.Attempts)
	c.logger.Info("retrying job", "job_id", job.JobID, "attempt", job.Attempts, "backoff", backoff)

	c.cache.AckJob(ctx, symbol, raw)

	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}

	updated, _ := json.Marshal(job)
	c.cache.EnqueueJob(ctx, symbol, updated)
}

func (c *Consumer) backoff(attempt int) time.Duration {
	base := float64(c.cfg.BaseBackoff)
	exp := math.Pow(2, float64(attempt))
	jitter := float64(rand.Intn(500)) * float64(time.Millisecond)
	d := time.Duration(base*exp + jitter)
	if d > c.cfg.MaxBackoff {
		return c.cfg.MaxBackoff
	}
	return d
}

// runCleanup periodically re-queues jobs stuck in the processing list.
// This handles worker crashes mid-delivery.
func (c *Consumer) runCleanup(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.RLock()
			syms := make([]string, len(c.symbols))
			copy(syms, c.symbols)
			c.mu.RUnlock()
			for _, sym := range syms {
				c.reQueueStale(ctx, sym)
			}
		}
	}
}

func (c *Consumer) reQueueStale(ctx context.Context, symbol string) {
	jobs, err := c.cache.GetAllProcessingJobs(ctx, symbol)
	if err != nil {
		c.logger.Error("get processing jobs", "symbol", symbol, "err", err)
		return
	}
	for _, jobStr := range jobs {
		var job signal.Job
		if err := json.Unmarshal([]byte(jobStr), &job); err != nil {
			continue
		}
		if time.Since(job.EnqueuedAt) > c.cfg.StaleJobThreshold {
			c.logger.Warn("re-queuing stale job", "job_id", job.JobID, "symbol", symbol)
			c.cache.NackJob(ctx, symbol, []byte(jobStr))
		}
	}
}

// ConsumeOne is the exported entry point called by the symbol manager.
// It runs a blocking consume loop for a single symbol until ctx is cancelled.
func (c *Consumer) ConsumeOne(ctx context.Context, symbol string) {
	c.consumeSymbol(ctx, symbol)
}

// ConsumeOne starts a consumer for a single symbol in the calling goroutine.
// Used when a symbol is added at runtime after Start() is already running.
