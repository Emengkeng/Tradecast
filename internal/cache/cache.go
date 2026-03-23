package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps two Redis connections with different eviction policies.
// Critical: noeviction — signal cache, queues, dedup, symbol list
// Cache:    allkeys-lru — api key cache, rate limits, machine binding, jwt blocklist
type Client struct {
	Critical *redis.Client
	Cache    *redis.Client
}

func New(criticalAddr, cacheAddr, password string) *Client {
	return &Client{
		Critical: redis.NewClient(&redis.Options{Addr: criticalAddr, Password: password, DB: 0}),
		Cache:    redis.NewClient(&redis.Options{Addr: cacheAddr, Password: password, DB: 0}),
	}
}

func (c *Client) Ping(ctx context.Context) error {
	if err := c.Critical.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("critical redis: %w", err)
	}
	if err := c.Cache.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache redis: %w", err)
	}
	return nil
}

// ---- Signal cache (critical) ----

func signalKey(symbol string) string { return fmt.Sprintf("signal:%s:latest", symbol) }

func (c *Client) SetLatestSignal(ctx context.Context, symbol string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.Critical.Set(ctx, signalKey(symbol), b, 0).Err()
}

func (c *Client) GetLatestSignal(ctx context.Context, symbol string) ([]byte, error) {
	b, err := c.Critical.Get(ctx, signalKey(symbol)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return b, err
}

// ---- Deduplication (critical) ----

func dedupKey(ticketID int64, signalType string) string {
	return fmt.Sprintf("dedup:%d:%s", ticketID, signalType)
}

// SetDedup returns true if new (not seen before).
func (c *Client) SetDedup(ctx context.Context, ticketID int64, signalType string, ttl time.Duration) (bool, error) {
	return c.Critical.SetNX(ctx, dedupKey(ticketID, signalType), 1, ttl).Result()
}

// ---- Queue — BRPOPLPUSH pattern (critical) ----

func QueueKey(symbol string) string      { return fmt.Sprintf("queue:signal:%s", symbol) }
func ProcessingKey(symbol string) string { return fmt.Sprintf("processing:signal:%s", symbol) }

const DeadLetterKey = "queue:dead_letter"

func (c *Client) EnqueueJob(ctx context.Context, symbol string, job any) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return c.Critical.LPush(ctx, QueueKey(symbol), b).Err()
}

func (c *Client) DequeueJob(ctx context.Context, symbol string, timeout time.Duration) ([]byte, error) {
	result, err := c.Critical.BRPopLPush(ctx, QueueKey(symbol), ProcessingKey(symbol), timeout).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return result, err
}

func (c *Client) AckJob(ctx context.Context, symbol string, jobData []byte) error {
	return c.Critical.LRem(ctx, ProcessingKey(symbol), 1, jobData).Err()
}

func (c *Client) NackJob(ctx context.Context, symbol string, jobData []byte) error {
	pipe := c.Critical.Pipeline()
	pipe.LRem(ctx, ProcessingKey(symbol), 1, jobData)
	pipe.LPush(ctx, QueueKey(symbol), jobData)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *Client) MoveToDeadLetter(ctx context.Context, symbol string, jobData []byte) error {
	pipe := c.Critical.Pipeline()
	pipe.LRem(ctx, ProcessingKey(symbol), 1, jobData)
	pipe.LPush(ctx, DeadLetterKey, jobData)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *Client) GetAllProcessingJobs(ctx context.Context, symbol string) ([]string, error) {
	return c.Critical.LRange(ctx, ProcessingKey(symbol), 0, -1).Result()
}

func (c *Client) QueueDepth(ctx context.Context, symbol string) (int64, error) {
	return c.Critical.LLen(ctx, QueueKey(symbol)).Result()
}

func (c *Client) DeadLetterDepth(ctx context.Context) (int64, error) {
	return c.Critical.LLen(ctx, DeadLetterKey).Result()
}

// ---- Symbol list cache (critical, noeviction) ----

const symbolCacheKey = "watched:symbols"

func (c *Client) SetSymbolList(ctx context.Context, symbols []string, ttl time.Duration) error {
	b, err := json.Marshal(symbols)
	if err != nil {
		return err
	}
	return c.Critical.Set(ctx, symbolCacheKey, b, ttl).Err()
}

func (c *Client) GetSymbolList(ctx context.Context) ([]string, error) {
	b, err := c.Critical.Get(ctx, symbolCacheKey).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var syms []string
	if err := json.Unmarshal(b, &syms); err != nil {
		return nil, err
	}
	return syms, nil
}

func (c *Client) InvalidateSymbolCache(ctx context.Context) error {
	return c.Critical.Del(ctx, symbolCacheKey).Err()
}

// InvalidateSymbolList is an alias for InvalidateSymbolCache.
func (c *Client) InvalidateSymbolList(ctx context.Context) error {
	return c.InvalidateSymbolCache(ctx)
}

// ---- API key cache (lru) ----

func apiKeyCacheKey(keyHash string) string { return fmt.Sprintf("apikey:%s", keyHash) }

func (c *Client) SetAPIKeyCache(ctx context.Context, keyHash string, data any, ttl time.Duration) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.Cache.Set(ctx, apiKeyCacheKey(keyHash), b, ttl).Err()
}

func (c *Client) GetAPIKeyCache(ctx context.Context, keyHash string) ([]byte, error) {
	b, err := c.Cache.Get(ctx, apiKeyCacheKey(keyHash)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return b, err
}

func (c *Client) DeleteAPIKeyCache(ctx context.Context, keyHash string) error {
	return c.Cache.Del(ctx, apiKeyCacheKey(keyHash)).Err()
}

// ---- Rate limiting (lru) ----

func rateLimitKey(ip string) string { return fmt.Sprintf("ratelimit:authfail:%s", ip) }

func (c *Client) IncrAuthFailure(ctx context.Context, ip string, window time.Duration) (int64, error) {
	key := rateLimitKey(ip)
	pipe := c.Cache.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, window)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

func (c *Client) GetAuthFailures(ctx context.Context, ip string) (int64, error) {
	n, err := c.Cache.Get(ctx, rateLimitKey(ip)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return n, err
}

// ---- JWT blocklist (lru) ----

func jwtBlockKey(jti string) string { return fmt.Sprintf("jwt:block:%s", jti) }

func (c *Client) BlockJWT(ctx context.Context, jti string, ttl time.Duration) error {
	return c.Cache.Set(ctx, jwtBlockKey(jti), 1, ttl).Err()
}

func (c *Client) IsJWTBlocked(ctx context.Context, jti string) (bool, error) {
	n, err := c.Cache.Exists(ctx, jwtBlockKey(jti)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- Machine binding cache (lru) ----
// Caches per-key machine authorization to avoid DB hits on every poll.
// Values: "1" = allowed, "0" = denied, missing = not cached.

const MachineCacheTTL = 5 * time.Minute

func machineCacheKey(keyID, accountNumber string) string {
	return fmt.Sprintf("machine:%s:%s", keyID, accountNumber)
}

// GetMachineAllowed returns (allowed, found).
func (c *Client) GetMachineAllowed(ctx context.Context, keyID, accountNumber string) (allowed bool, found bool, err error) {
	val, e := c.Cache.Get(ctx, machineCacheKey(keyID, accountNumber)).Result()
	if e == redis.Nil {
		return false, false, nil
	}
	if e != nil {
		return false, false, e
	}
	return val == "1", true, nil
}

func (c *Client) SetMachineAllowed(ctx context.Context, keyID, accountNumber string, allowed bool, ttl time.Duration) error {
	val := "0"
	if allowed {
		val = "1"
	}
	return c.Cache.Set(ctx, machineCacheKey(keyID, accountNumber), val, ttl).Err()
}

// InvalidateMachineCache removes all cached machine entries for a key.
func (c *Client) InvalidateMachineCache(ctx context.Context, keyID string) error {
	pattern := fmt.Sprintf("machine:%s:*", keyID)
	var cursor uint64
	for {
		keys, next, err := c.Cache.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			c.Cache.Del(ctx, keys...)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}
