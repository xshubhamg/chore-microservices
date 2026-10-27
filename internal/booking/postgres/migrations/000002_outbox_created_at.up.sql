-- The relay's backlog age must reflect when an event became pending, not when it
-- is next eligible for retry. available_at moves into the future on every retry
-- (and on claim), so it cannot be used to detect a stuck relay.
ALTER TABLE outbox_messages
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
