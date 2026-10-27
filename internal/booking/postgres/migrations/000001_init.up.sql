-- Booking service schema.
--
-- The service owns its database (database-per-service). It stores a snapshot of
-- the catalog service so a booking remains historically correct even when the
-- catalog price or name changes later.

CREATE TABLE bookings (
    id                    BIGSERIAL PRIMARY KEY,
    service_id            BIGINT       NOT NULL,
    service_name          TEXT         NOT NULL,
    service_price_cents   BIGINT       NOT NULL CHECK (service_price_cents >= 0),
    duration_minutes      INTEGER      NOT NULL CHECK (duration_minutes > 0),
    customer_name         TEXT         NOT NULL,
    customer_email        TEXT         NOT NULL,
    scheduled_at          TIMESTAMPTZ  NOT NULL,
    status                TEXT         NOT NULL
        CHECK (status IN ('pending', 'confirmed', 'completed', 'cancelled')),
    cancellation_reason   TEXT,
    version               INTEGER      NOT NULL DEFAULT 1,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX bookings_status_id_idx ON bookings (status, id);
CREATE INDEX bookings_customer_email_idx ON bookings (customer_email);

-- Transactional outbox: written in the same transaction as the booking.
CREATE TABLE outbox_messages (
    id           BIGSERIAL PRIMARY KEY,
    event_id     UUID         NOT NULL UNIQUE,
    event_type   TEXT         NOT NULL,
    payload      JSONB        NOT NULL,
    trace_parent TEXT,
    attempts     INTEGER      NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    claimed_at   TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    failed_at    TIMESTAMPTZ,
    last_error   TEXT
);

-- Partial index keeps the relay's claim query cheap as the table grows.
CREATE INDEX outbox_pending_idx
    ON outbox_messages (available_at, id)
    WHERE published_at IS NULL AND failed_at IS NULL;

CREATE INDEX outbox_published_idx
    ON outbox_messages (published_at)
    WHERE published_at IS NOT NULL;

-- Idempotency-Key support for POST /v1/bookings.
CREATE TABLE idempotency_keys (
    key             TEXT PRIMARY KEY,
    request_hash    TEXT        NOT NULL,
    state           TEXT        NOT NULL DEFAULT 'in_progress'
        CHECK (state IN ('in_progress', 'completed')),
    response_status INTEGER,
    response_body   JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX idempotency_expires_idx ON idempotency_keys (expires_at);
