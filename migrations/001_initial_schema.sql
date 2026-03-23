-- 001_initial_schema.sql

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS api_keys (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash         TEXT NOT NULL UNIQUE,
    owner            TEXT NOT NULL,
    scopes           TEXT[] NOT NULL DEFAULT '{}',
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','suspended')),
    successor_key_id UUID REFERENCES api_keys(id),
    rotate_at        TIMESTAMPTZ,
    note             TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at     TIMESTAMPTZ
);

CREATE INDEX idx_api_keys_hash   ON api_keys(key_hash);
CREATE INDEX idx_api_keys_status ON api_keys(status);

CREATE TABLE IF NOT EXISTS signals (
    id          UUID NOT NULL DEFAULT gen_random_uuid(),
    ticket_id   BIGINT NOT NULL,
    signal_type TEXT NOT NULL CHECK (signal_type IN ('OPEN','MODIFY','CLOSE','PARTIAL')),
    symbol      TEXT NOT NULL,
    direction   TEXT NOT NULL CHECK (direction IN ('BUY','SELL')),
    price       NUMERIC(20,8) NOT NULL,
    sl          NUMERIC(20,8),
    tp          NUMERIC(20,8),
    lot         NUMERIC(20,4) NOT NULL,
    source_key_id UUID REFERENCES api_keys(id),
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, received_at)
) PARTITION BY RANGE (received_at);

-- Partitions for 2026 and 2027
DO $$
DECLARE
    y INT;
    m INT;
    start_date DATE;
    end_date DATE;
    tname TEXT;
BEGIN
    FOR y IN 2025..2027 LOOP
        FOR m IN 1..12 LOOP
            start_date := make_date(y, m, 1);
            end_date   := start_date + INTERVAL '1 month';
            tname      := format('signals_%s_%s', y, lpad(m::text, 2, '0'));
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF signals FOR VALUES FROM (%L) TO (%L)',
                tname, start_date, end_date
            );
        END LOOP;
    END LOOP;
END$$;

CREATE INDEX idx_signals_ticket  ON signals(ticket_id, received_at);
CREATE INDEX idx_signals_symbol  ON signals(symbol, received_at);
CREATE INDEX idx_signals_type    ON signals(signal_type, received_at);

CREATE TABLE IF NOT EXISTS subscribers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_id     UUID NOT NULL REFERENCES api_keys(id),
    channel    TEXT NOT NULL CHECK (channel IN ('telegram','whatsapp','webhook','mt4')),
    config     JSONB NOT NULL DEFAULT '{}',
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_subscribers_key_id ON subscribers(key_id);
CREATE INDEX idx_subscribers_active ON subscribers(active);

CREATE TABLE IF NOT EXISTS delivery_log (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    signal_id         UUID NOT NULL,
    subscriber_id     UUID NOT NULL REFERENCES subscribers(id),
    channel           TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('pending','delivered','failed','dead')),
    attempts          INT NOT NULL DEFAULT 0,
    last_attempted_at TIMESTAMPTZ,
    error             TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_delivery_signal     ON delivery_log(signal_id);
CREATE INDEX idx_delivery_status     ON delivery_log(status);
CREATE INDEX idx_delivery_subscriber ON delivery_log(subscriber_id);

CREATE TABLE IF NOT EXISTS dead_letter (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_payload    JSONB NOT NULL,
    signal_id      UUID,
    failure_reason TEXT NOT NULL,
    failed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
