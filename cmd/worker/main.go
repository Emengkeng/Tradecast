package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/config"
	"github.com/mt4signal/internal/notify/mt4copier"
	"github.com/mt4signal/internal/notify/telegram"
	"github.com/mt4signal/internal/notify/webhook"
	"github.com/mt4signal/internal/notify/whatsapp"
	"github.com/mt4signal/internal/queue"
	"github.com/mt4signal/internal/store"
	"github.com/mt4signal/internal/symbols"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := config.Load()

	st, err := store.New(cfg.Postgres.DSN, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns)
	if err != nil {
		logger.Error("postgres init", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	c := cache.New(cfg.Redis.CriticalAddr, cfg.Redis.CacheAddr, cfg.Redis.Password)
	if err := c.Ping(context.Background()); err != nil {
		logger.Error("redis init", "err", err)
		os.Exit(1)
	}

	if err := st.Migrate(context.Background(), "migrations"); err != nil {
		logger.Error("migration failed", "err", err)
		os.Exit(1)
	}
	logger.Info("migrations applied")

	logger.Info("worker starting")

	// Build notification handlers
	tgHandler := telegram.New(cfg.Notify.TelegramToken, cfg.Notify.TelegramBaseURL, st)
	waHandler := whatsapp.New(cfg.Notify, st)
	wbHandler := webhook.New(st)
	mt4Handler := mt4copier.New(c, st, logger)

	// Build consumer
	consumer := queue.NewConsumer(c, st, cfg.Worker, logger)
	consumer.RegisterHandler(tgHandler.Handle)
	consumer.RegisterHandler(waHandler.Handle)
	consumer.RegisterHandler(wbHandler.Handle)
	consumer.RegisterHandler(mt4Handler.Handle)

	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		logger.Info("shutdown signal received")
		cancel()
	}()

	// Symbol manager: loads from DB/cache (with hardcoded fallback),
	// starts per-symbol consumer goroutines, and syncs every 5 min.
	symManager := symbols.NewManager(st, c, consumer, logger)
	symManager.Start(ctx)

	logger.Info("worker stopped")
}
