ALTER TABLE outbox_messages
    DROP COLUMN IF EXISTS created_at;
