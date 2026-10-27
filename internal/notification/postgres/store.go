package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xshubhamg/chore-microservices/internal/notification/app"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) IsProcessed(ctx context.Context, eventID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1::uuid)
	`, eventID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check processed event: %w", err)
	}
	return exists, nil
}

// MarkProcessed records the event and the audit row in one transaction. The
// unique event_id is the deduplication guard.
func (s *Store) MarkProcessed(ctx context.Context, envelope events.Envelope, notification app.Notification) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id, event_type)
		VALUES ($1::uuid, $2)
		ON CONFLICT (event_id) DO NOTHING
	`, envelope.ID, envelope.Type)
	if err != nil {
		return fmt.Errorf("insert processed event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO notifications (
			event_id, booking_id, kind, recipient, subject, body, sent_at
		) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
	`,
		notification.EventID,
		notification.BookingID,
		notification.Kind,
		notification.Recipient,
		notification.Subject,
		notification.Body,
		notification.SentAt,
	)
	if err != nil {
		return fmt.Errorf("insert notification audit row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit processed event: %w", err)
	}
	return nil
}
