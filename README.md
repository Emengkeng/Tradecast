# MT4Signal

Production-grade MT4 trade signal broadcaster. When you enter a trade on MT4, it broadcasts to Telegram, WhatsApp, webhooks, and copies to other MT4 accounts in under 500ms.

## Architecture

```
MT4 Monitor EA (HMAC signed)
        ↓
Go HTTP Server (VPS)
  ├── HMAC + timestamp verification (only your MT4 can send signals)
  ├── JWT admin auth
  ├── POST /signal → dedup → Postgres → Redis cache → job queue
  ├── GET /pending/{symbol} → Redis (receiver EAs poll this)
  └── Admin API + Web Dashboard
        ↓
Go Worker (separate process)
  ├── Per-symbol sequential queues (ordered delivery)
  ├── Exponential backoff + jitter on failure
  ├── Dead letter queue after max retries
  └── Stale job cleanup (crash recovery)
        ↓
Postgres (source of truth) + Redis (speed layer)
```

## Security

- **Signal ingestion**: Every POST to `/signal` must carry a valid HMAC-SHA256 signature. The EA signs `ticket_id:signal_type:symbol:timestamp` with a shared secret. Requests older than 30 seconds are rejected (replay attack prevention). No valid signature = 401.
- **Admin API**: JWT access + refresh tokens. 15-minute access token TTL. Refresh tokens are blocklisted on logout.
- **API keys**: SHA-256 hashed in DB, never stored plain. Supports active/suspended/revoked states. Rotation with overlap window.
- **Rate limiting**: Per-IP auth failure counter in Redis.

## Quick Start

### 1. Prerequisites

- Docker + Docker Compose
- Go 1.22+ (for local development)

### 2. Clone and configure

```bash
cp .env.example .env
```

Edit `.env`:

**Generate SIGNAL_HMAC_SECRET:**
```bash
openssl rand -hex 32
```

**Generate JWT_SECRET:**
```bash
openssl rand -hex 32
```

**Generate ADMIN_PASSWORD_HASH** (bcrypt of your chosen password):
```bash
# Install htpasswd (apache2-utils) or use this Go one-liner:
go run -mod=mod -ldflags="" <(cat <<'EOF'
package main
import ("fmt"; "golang.org/x/crypto/bcrypt"; "os")
func main() {
    h, _ := bcrypt.GenerateFromPassword([]byte(os.Args[1]), 12)
    fmt.Println(string(h))
}
EOF
) yourpassword
```

### 3. Start everything

```bash
cd docker
docker compose up -d
```

Server starts on port 8080. Admin dashboard at http://your-vps-ip:8080/dashboard

### 4. MT4 Setup

**Monitor EA (your trading account):**
1. Open MT4 → Tools → Options → Expert Advisors
2. Tick "Allow WebRequest for listed URL"
3. Add your server URL: `http://your-vps-ip:8080/signal`
4. Compile `mql4/monitor_ea.mq4` in MetaEditor
5. Attach to any chart (it runs in background)
6. Set `HMACSecret` input to match `SIGNAL_HMAC_SECRET` in your `.env`

**Receiver EA (copy account):**
1. Whitelist `http://your-vps-ip:8080` in MT4 options
2. Compile `mql4/receiver_ea.mq4`
3. Issue an API key from the admin dashboard (scope: `copy:receive`)
4. Attach EA to a chart for the symbol you want to copy
5. Set `APIKey` and `SymbolToWatch` inputs

### 5. Add subscribers via dashboard

Open http://your-vps-ip:8080/dashboard, log in, then:
1. **API Keys** → Issue Key → set owner + scopes
2. **Subscribers** → Add Subscriber → pick channel + paste config

**Telegram config:** `{"chat_id": "-1001234567890"}`  
Get chat_id by adding your bot to a group and calling `getUpdates`.

**WhatsApp (Twilio) config:** `{"phone": "+237612345678"}`

**Webhook config:** `{"url": "https://your-server.com/hook", "secret": "optional-hmac-secret"}`

**MT4 copier config:** `{"symbols": ["EURUSD", "XAUUSD"]}` (empty = all symbols)

## API Reference

### Auth
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | `/auth/login` | — | Returns access + refresh tokens |
| POST | `/auth/refresh` | — | Refresh access token |
| POST | `/auth/logout` | JWT | Revoke refresh token |

### Signal (MT4 → Server)
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | `/signal` | HMAC headers | Ingest signal from monitor EA |
| GET | `/pending/{symbol}` | X-API-Key | Poll latest signal (receiver EA) |

### Admin (JWT required)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/admin/keys` | Issue new API key |
| GET | `/admin/keys` | List all keys |
| PATCH | `/admin/keys/{id}/status` | Set active/revoked/suspended |
| POST | `/admin/keys/{id}/rotate` | Rotate key with overlap window |
| POST | `/admin/subscribers` | Add subscriber |
| GET | `/admin/subscribers` | List subscribers |
| PATCH | `/admin/subscribers/{id}/active` | Enable/disable subscriber |
| GET | `/admin/metrics` | Dashboard metrics |
| GET | `/admin/signals` | Signal history |

### System
| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Health check (Postgres + Redis) |

## Signal Payload (MT4 → Server)

```json
POST /signal
X-Signal-Signature: <hmac-sha256-hex>
X-Signal-Timestamp: 2024-01-15T10:30:00Z

{
  "ticket_id": 12345678,
  "signal_type": "OPEN",
  "symbol": "EURUSD",
  "direction": "BUY",
  "price": 1.08542,
  "sl": 1.08200,
  "tp": 1.09000,
  "lot": 0.10,
  "timestamp": "2024-01-15T10:30:00Z"
}
```

Signal types: `OPEN`, `MODIFY`, `CLOSE`, `PARTIAL`

HMAC message format: `ticket_id:signal_type:SYMBOL:timestamp`

## Scaling

- Run multiple `server` instances behind a load balancer (stateless, share Postgres + Redis)
- Run multiple `worker` instances (each picks jobs from Redis queues)
- Add new notification channels: implement `func(ctx, *signal.Job) error` in `internal/notify/`, register in `cmd/worker/main.go`
- Add new symbols: add to `watchedSymbols` in `cmd/worker/main.go`

## Project Structure

```
cmd/
  server/main.go       HTTP server entrypoint
  worker/main.go       Worker process entrypoint
internal/
  auth/                HMAC verification, API key validation, JWT admin
  signal/              Signal receive, dedup, persist, enqueue + pending poll
  queue/               BRPOPLPUSH consumer, retry, dead letter, stale cleanup
  store/               Postgres layer
  cache/               Redis layer (two clients: critical + lru)
  admin/               Admin HTTP handlers
  config/              Env-based config
  health/              Health check
  notify/
    telegram/          Telegram Bot API
    whatsapp/          Twilio / CallMeBot
    webhook/           HTTP webhook with HMAC signing
    mt4copier/         MT4 receiver delivery log
migrations/
  001_initial_schema.sql
mql4/
  monitor_ea.mq4       Runs on your trading MT4
  receiver_ea.mq4      Runs on copy MT4 accounts
web/
  dashboard.html       Admin web UI (single file)
docker/
  docker-compose.yml
  Dockerfile.server
  Dockerfile.worker
```
