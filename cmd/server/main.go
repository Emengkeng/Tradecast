package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mt4signal/internal/admin"
	"github.com/mt4signal/internal/auth"
	"github.com/mt4signal/internal/cache"
	"github.com/mt4signal/internal/config"
	"github.com/mt4signal/internal/health"
	signalpkg "github.com/mt4signal/internal/signal"
	"github.com/mt4signal/internal/store"
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
	logger.Info("postgres connected")

	c := cache.New(cfg.Redis.CriticalAddr, cfg.Redis.CacheAddr, cfg.Redis.Password)
	if err := c.Ping(context.Background()); err != nil {
		logger.Error("redis init", "err", err)
		os.Exit(1)
	}
	logger.Info("redis connected")

	authSvc := auth.NewService(st, c, cfg.Auth, logger)
	sigHandler := signalpkg.NewHandler(st, c, cfg.Auth, logger)
	adminHandler := admin.New(st, c, authSvc, logger)
	healthHandler := health.New(st, c)

	mux := http.NewServeMux()

	// Health — no auth
	mux.HandleFunc("GET /health", healthHandler.Check)

	// Auth endpoints — public
	mux.HandleFunc("POST /auth/login", adminHandler.Login)
	mux.HandleFunc("POST /auth/refresh", adminHandler.Refresh)
	mux.Handle("POST /auth/logout", authSvc.JWTMiddleware(http.HandlerFunc(adminHandler.Logout)))

	// Signal ingestion — HMAC verified inside handler (no master key needed in header)
	mux.HandleFunc("POST /signal", sigHandler.Receive)

	// Receiver EA polling — API key required with copy:receive scope
	mux.Handle("GET /pending/{symbol}",
		authSvc.APIKeyMiddleware(auth.ScopeCopyReceive)(
			http.HandlerFunc(sigHandler.GetPending),
		),
	)

	// Admin endpoints — JWT required
	adminMW := authSvc.JWTMiddleware
	mux.Handle("POST /admin/keys", adminMW(http.HandlerFunc(adminHandler.IssueKey)))
	mux.Handle("GET /admin/keys", adminMW(http.HandlerFunc(adminHandler.ListKeys)))
	mux.Handle("PATCH /admin/keys/{id}/status", adminMW(http.HandlerFunc(adminHandler.SetKeyStatus)))
	mux.Handle("POST /admin/keys/{id}/rotate", adminMW(http.HandlerFunc(adminHandler.RotateKey)))
	mux.Handle("POST /admin/subscribers", adminMW(http.HandlerFunc(adminHandler.CreateSubscriber)))
	mux.Handle("GET /admin/subscribers", adminMW(http.HandlerFunc(adminHandler.ListSubscribers)))
	mux.Handle("PATCH /admin/subscribers/{id}/active", adminMW(http.HandlerFunc(adminHandler.SetSubscriberActive)))
	mux.Handle("GET /admin/metrics", adminMW(http.HandlerFunc(adminHandler.GetMetrics)))
	mux.Handle("GET /admin/signals", adminMW(http.HandlerFunc(adminHandler.ListSignals)))

	// Machine binding management
	mux.Handle("GET /admin/keys/{id}/machines", adminMW(http.HandlerFunc(adminHandler.ListMachines)))
	mux.Handle("DELETE /admin/keys/{id}/machines/{account}", adminMW(http.HandlerFunc(adminHandler.RemoveMachine)))
	mux.Handle("PATCH /admin/keys/{id}/machines", adminMW(http.HandlerFunc(adminHandler.SetMaxMachines)))

	// Symbol management
	mux.Handle("GET /admin/symbols", adminMW(http.HandlerFunc(adminHandler.ListSymbols)))
	mux.Handle("POST /admin/symbols", adminMW(http.HandlerFunc(adminHandler.AddSymbol)))
	mux.Handle("PATCH /admin/symbols/{symbol}", adminMW(http.HandlerFunc(adminHandler.SetSymbolActive)))
	mux.Handle("DELETE /admin/symbols/{symbol}", adminMW(http.HandlerFunc(adminHandler.DeleteSymbol)))

	// Machine binding management
	mux.Handle("GET /admin/machines", adminMW(http.HandlerFunc(adminHandler.ListAllMachines)))
	mux.Handle("GET /admin/keys/{id}/machines", adminMW(http.HandlerFunc(adminHandler.ListKeyMachines)))
	mux.Handle("DELETE /admin/keys/{id}/machines/{account}", adminMW(http.HandlerFunc(adminHandler.RemoveMachine)))
	mux.Handle("PATCH /admin/keys/{id}/max-machines", adminMW(http.HandlerFunc(adminHandler.SetKeyMaxMachines)))

	// Symbol management
	mux.Handle("GET /admin/symbols", adminMW(http.HandlerFunc(adminHandler.ListSymbols)))
	mux.Handle("POST /admin/symbols", adminMW(http.HandlerFunc(adminHandler.AddSymbol)))
	mux.Handle("PATCH /admin/symbols/{symbol}", adminMW(http.HandlerFunc(adminHandler.SetSymbolActive)))
	mux.Handle("DELETE /admin/symbols/{symbol}", adminMW(http.HandlerFunc(adminHandler.DeleteSymbol)))

	// Machine management
	mux.Handle("GET /admin/keys/{id}/machines", adminMW(http.HandlerFunc(adminHandler.ListMachines)))
	mux.Handle("DELETE /admin/keys/{id}/machines/{account}", adminMW(http.HandlerFunc(adminHandler.RemoveMachine)))
	mux.Handle("PATCH /admin/keys/{id}/max-machines", adminMW(http.HandlerFunc(adminHandler.SetMaxMachines)))

	// Symbol management
	mux.Handle("GET /admin/symbols", adminMW(http.HandlerFunc(adminHandler.ListSymbols)))
	mux.Handle("POST /admin/symbols", adminMW(http.HandlerFunc(adminHandler.AddSymbol)))
	mux.Handle("PATCH /admin/symbols/{symbol}", adminMW(http.HandlerFunc(adminHandler.SetSymbolActive)))
	mux.Handle("DELETE /admin/symbols/{symbol}", adminMW(http.HandlerFunc(adminHandler.DeleteSymbol)))

	// Serve admin web UI
	mux.Handle("GET /dashboard", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/dashboard.html")
	}))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))

	srv := &http.Server{
		Addr:         ":" + cfg.Server.Port,
		Handler:      loggingMiddleware(logger)(corsMiddleware(mux)),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("server starting", "port", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-quit
	logger.Info("shutdown signal received")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("server stopped")
}

func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &rw{ResponseWriter: w, status: 200}
			next.ServeHTTP(rw, r)
			logger.Info("req",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type rw struct {
	http.ResponseWriter
	status int
}

func (r *rw) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }
