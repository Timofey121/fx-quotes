CREATE TABLE quote_updates (
    id UUID PRIMARY KEY,
    request_seq BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    pair TEXT NOT NULL CHECK (pair IN ('USD/EUR','USD/MXN','EUR/USD','EUR/MXN','MXN/USD','MXN/EUR')),
    idempotency_key TEXT COLLATE "C" UNIQUE CHECK (idempotency_key <> '' AND octet_length(idempotency_key) <= 128),
    status TEXT NOT NULL DEFAULT 'pending',
    attempt BIGINT NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    next_attempt_at TIMESTAMPTZ DEFAULT clock_timestamp(),
    lease_until TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    rate NUMERIC(30,15) CHECK (rate > 0 AND rate <> 'NaN'::numeric),
    provider TEXT CHECK (provider <> ''),
    source_date DATE,
    failure_code TEXT CHECK (failure_code IN ('provider_permanent','attempts_exhausted')),
    CONSTRAINT update_state_shape CHECK (
        (status = 'pending' AND next_attempt_at IS NOT NULL AND lease_until IS NULL
            AND finished_at IS NULL AND rate IS NULL AND provider IS NULL AND source_date IS NULL AND failure_code IS NULL)
        OR (status = 'processing' AND attempt > 0 AND next_attempt_at IS NULL AND lease_until IS NOT NULL
            AND finished_at IS NULL AND rate IS NULL AND provider IS NULL AND source_date IS NULL AND failure_code IS NULL)
        OR (status = 'completed' AND attempt > 0 AND next_attempt_at IS NULL AND lease_until IS NULL
            AND finished_at IS NOT NULL AND rate IS NOT NULL AND provider IS NOT NULL AND source_date IS NOT NULL AND failure_code IS NULL)
        OR (status = 'failed' AND attempt > 0 AND next_attempt_at IS NULL AND lease_until IS NULL
            AND finished_at IS NOT NULL AND rate IS NULL AND provider IS NULL AND source_date IS NULL AND failure_code IS NOT NULL)
    )
);

CREATE INDEX quote_updates_ready ON quote_updates (next_attempt_at, request_seq) WHERE status = 'pending';
CREATE INDEX quote_updates_expired ON quote_updates (lease_until) WHERE status = 'processing';
CREATE INDEX quote_updates_active ON quote_updates (id) WHERE status IN ('pending','processing');

CREATE TABLE latest_quotes (
    pair TEXT PRIMARY KEY CHECK (pair IN ('USD/EUR','USD/MXN','EUR/USD','EUR/MXN','MXN/USD','MXN/EUR')),
    update_id UUID NOT NULL REFERENCES quote_updates(id),
    request_seq BIGINT NOT NULL,
    rate NUMERIC(30,15) NOT NULL CHECK (rate > 0 AND rate <> 'NaN'::numeric),
    provider TEXT NOT NULL CHECK (provider <> ''),
    source_date DATE NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
