package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xshubhamg/chore-microservices/internal/platform/outbox"
)

// OutboxStore is the Postgres adapter for the outbox relay.
type OutboxStore struct {
	pool *pgxpool.Pool
}

func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

// Claim takes a batch of pending messages and leases them for the given
// duration. SKIP LOCKED lets several relay replicas run without coordination.
func (s *OutboxStore) Claim(ctx context.Context, limit int, lease time.Duration) ([]outbox.Message, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE outbox_messages
		SET attempts = attempts + 1,
		    claimed_at = now(),
		    available_at = now() + make_interval(secs => $2)
		WHERE id IN (
			SELECT id
			FROM outbox_messages
			WHERE published_at IS NULL
			  AND failed_at IS NULL
			  AND available_at <= now()
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, event_id::text, event_type, payload, coalesce(trace_parent, ''), attempts
	`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()

	messages := make([]outbox.Message, 0, limit)
	for rows.Next() {
		var message outbox.Message
		if err := rows.Scan(
			&message.ID,
			&message.EventID,
			&message.EventType,
			&message.Payload,
			&message.TraceParent,
			&message.Attempts,
		); err != nil {
			return nil, fmt.Errorf("scan outbox message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox messages: %w", err)
	}
	return messages, nil
}

func (s *OutboxStore) MarkPublished(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_messages
		SET published_at = now(), last_error = NULL
		WHERE id = $1
	`, id)
	return err
}

func (s *OutboxStore) MarkRetry(ctx context.Context, id int64, retryAt time.Time, cause string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_messages
		SET available_at = $2, last_error = $3
		WHERE id = $1
	`, id, retryAt, cause)
	return err
}

func (s *OutboxStore) MarkDead(ctx context.Context, id int64, cause string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_messages
		SET failed_at = now(), last_error = $2
		WHERE id = $1
	`, id, cause)
	return err
}

func (s *OutboxStore) Pending(ctx context.Context) (int64, time.Duration, error) {
	var (
		count  int64
		oldest *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(available_at)
		FROM outbox_messages
		WHERE published_at IS NULL AND failed_at IS NULL
	`).Scan(&count, &oldest)
	if err != nil {
		return 0, 0, err
	}
	if count == 0 || oldest == nil {
		return count, 0, nil
	}
	return count, time.Since(*oldest), nil
}

func (s *OutboxStore) PurgePublished(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM outbox_messages WHERE published_at < $1`, olderThan)
	if err != nil {
		return 0, errors.Join(fmt.Errorf("purge published outbox messages"), err)
	}
	return tag.RowsAffected(), nil
}
