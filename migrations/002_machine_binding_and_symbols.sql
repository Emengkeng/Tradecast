-- 002_machine_binding_and_symbols.sql

-- Machine registrations per API key
CREATE TABLE IF NOT EXISTS key_machines (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_id         UUID NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    account_number TEXT NOT NULL,
    registered_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (key_id, account_number)
);

CREATE INDEX idx_key_machines_key_id  ON key_machines(key_id);
CREATE INDEX idx_key_machines_account ON key_machines(account_number);

-- max_machines: NULL = unlimited, 0 = none allowed, N = up to N machines
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS max_machines INT DEFAULT NULL;

-- Watched symbols for the worker
CREATE TABLE IF NOT EXISTS watched_symbols (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol     TEXT NOT NULL UNIQUE,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_watched_symbols_active ON watched_symbols(active);

-- Seed defaults
INSERT INTO watched_symbols (symbol) VALUES
    ('EURUSD'),('GBPUSD'),('USDJPY'),('XAUUSD'),
    ('USDCHF'),('AUDUSD'),('USDCAD'),('NZDUSD'),
    ('GBPJPY'),('EURJPY'),('XAGUSD'),('BTCUSD')
ON CONFLICT (symbol) DO NOTHING;
