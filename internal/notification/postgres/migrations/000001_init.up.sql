-- Notification service schema. The service keeps its own deduplication state
-- and an audit trail of everything it sent.

CREATE TABLE processed_events (
    event_id     UUID PRIMARY KEY,
    event_type   TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE notifications (
    id         BIGSERIAL PRIMARY KEY,
    event_id   UUID        NOT NULL,
    booking_id BIGINT      NOT NULL,
    kind       TEXT        NOT NULL,
    recipient  TEXT        NOT NULL,
    subject    TEXT        NOT NULL,
    body       TEXT        NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX notifications_booking_idx ON notifications (booking_id);
CREATE INDEX notifications_sent_at_idx ON notifications (sent_at DESC);
