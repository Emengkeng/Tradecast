package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server   ServerConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Auth     AuthConfig
	Worker   WorkerConfig
	Notify   NotifyConfig
}

type ServerConfig struct {
	Port         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

type PostgresConfig struct {
	DSN          string
	MaxOpenConns int
	MaxIdleConns int
}

type RedisConfig struct {
	CriticalAddr string
	CacheAddr    string
	Password     string
}

type AuthConfig struct {
	// Signal HMAC
	SignalHMACSecret   string
	SignalTimestampTTL time.Duration // reject signals older than this

	// API key cache
	KeyCacheTTL      time.Duration
	RateLimitWindow  time.Duration
	RateLimitMaxFail int
	DeduplicateTTL   time.Duration

	// JWT admin
	JWTSecret          string
	JWTAccessTokenTTL  time.Duration
	JWTRefreshTokenTTL time.Duration
	AdminUsername      string
	AdminPasswordHash  string // bcrypt hash of admin password
}

type WorkerConfig struct {
	Concurrency       int
	MaxRetries        int
	BaseBackoff       time.Duration
	MaxBackoff        time.Duration
	StaleJobThreshold time.Duration
	CleanupInterval   time.Duration
}

type NotifyConfig struct {
	TelegramToken    string
	TelegramBaseURL  string
	WhatsAppProvider string
	TwilioSID        string
	TwilioToken      string
	TwilioFrom       string
	CallMeBotAPIKey  string
}

func Load() *Config {
	return &Config{
		Server: ServerConfig{
			Port:         getEnv("SERVER_PORT", "8080"),
			ReadTimeout:  getDuration("SERVER_READ_TIMEOUT", 10*time.Second),
			WriteTimeout: getDuration("SERVER_WRITE_TIMEOUT", 10*time.Second),
		},
		Postgres: PostgresConfig{
			DSN:          mustEnv("POSTGRES_DSN"),
			MaxOpenConns: getInt("POSTGRES_MAX_OPEN_CONNS", 25),
			MaxIdleConns: getInt("POSTGRES_MAX_IDLE_CONNS", 10),
		},
		Redis: RedisConfig{
			CriticalAddr: getEnv("REDIS_CRITICAL_ADDR", "localhost:6379"),
			CacheAddr:    getEnv("REDIS_CACHE_ADDR", "localhost:6380"),
			Password:     getEnv("REDIS_PASSWORD", ""),
		},
		Auth: AuthConfig{
			SignalHMACSecret:   mustEnv("SIGNAL_HMAC_SECRET"),
			SignalTimestampTTL: getDuration("SIGNAL_TIMESTAMP_TTL", 30*time.Second),
			KeyCacheTTL:        getDuration("AUTH_KEY_CACHE_TTL", 5*time.Minute),
			RateLimitWindow:    getDuration("AUTH_RATE_LIMIT_WINDOW", 15*time.Minute),
			RateLimitMaxFail:   getInt("AUTH_RATE_LIMIT_MAX_FAIL", 10),
			DeduplicateTTL:     getDuration("AUTH_DEDUP_TTL", 60*time.Second),
			JWTSecret:          mustEnv("JWT_SECRET"),
			JWTAccessTokenTTL:  getDuration("JWT_ACCESS_TTL", 15*time.Minute),
			JWTRefreshTokenTTL: getDuration("JWT_REFRESH_TTL", 7*24*time.Hour),
			AdminUsername:      mustEnv("ADMIN_USERNAME"),
			AdminPasswordHash:  mustEnv("ADMIN_PASSWORD_HASH"),
		},
		Worker: WorkerConfig{
			Concurrency:       getInt("WORKER_CONCURRENCY", 10),
			MaxRetries:        getInt("WORKER_MAX_RETRIES", 5),
			BaseBackoff:       getDuration("WORKER_BASE_BACKOFF", 1*time.Second),
			MaxBackoff:        getDuration("WORKER_MAX_BACKOFF", 5*time.Minute),
			StaleJobThreshold: getDuration("WORKER_STALE_JOB_THRESHOLD", 60*time.Second),
			CleanupInterval:   getDuration("WORKER_CLEANUP_INTERVAL", 30*time.Second),
		},
		Notify: NotifyConfig{
			TelegramToken:    getEnv("TELEGRAM_BOT_TOKEN", ""),
			TelegramBaseURL:  getEnv("TELEGRAM_BASE_URL", "https://api.telegram.org"),
			WhatsAppProvider: getEnv("WHATSAPP_PROVIDER", "twilio"),
			TwilioSID:        getEnv("TWILIO_ACCOUNT_SID", ""),
			TwilioToken:      getEnv("TWILIO_AUTH_TOKEN", ""),
			TwilioFrom:       getEnv("TWILIO_FROM_NUMBER", ""),
			CallMeBotAPIKey:  getEnv("CALLMEBOT_API_KEY", ""),
		},
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("required env var not set: " + key)
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
