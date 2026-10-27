package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xshubhamg/chore-microservices/internal/booking/app"
	"github.com/xshubhamg/chore-microservices/internal/booking/domain"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
)

type Repository struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const bookingColumns = `
	id, service_id, service_name, service_price_cents, duration_minutes,
	customer_name, customer_email, scheduled_at, status,
	coalesce(cancellation_reason, ''), version, created_at, updated_at`

// Create writes the booking and its outbox event atomically. Either both rows
// exist or neither does, which is the core of the outbox pattern.
func (r *Repository) Create(ctx context.Context, booking *domain.Booking, buildEvent func() (events.Outbox, error)) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO bookings (
			service_id, service_name, service_price_cents, duration_minutes,
			customer_name, customer_email, scheduled_at, status, version
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at, updated_at
	`,
		booking.ServiceID,
		booking.ServiceName,
		booking.ServicePriceCents,
		booking.DurationMinutes,
		booking.CustomerName,
		booking.CustomerEmail,
		booking.ScheduledAt,
		booking.Status,
		booking.Version,
	).Scan(&booking.ID, &booking.CreatedAt, &booking.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert booking: %w", err)
	}

	// The booking ID now exists, so the event payload can reference it.
	event, err := buildEvent()
	if err != nil {
		return err
	}
	payload, err := event.Envelope.Marshal()
	if err != nil {
		return fmt.Errorf("marshal outbox event: %w", err)
	}

	if err := insertOutbox(ctx, tx, event, payload); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create booking: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id int64) (*domain.Booking, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id = $1`, id)
	booking, err := scanBooking(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ErrNotFound
		}
		return nil, fmt.Errorf("get booking: %w", err)
	}
	return booking, nil
}

func (r *Repository) List(ctx context.Context, filter app.ListFilter) ([]*domain.Booking, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+bookingColumns+`
		FROM bookings
		WHERE ($1::text IS NULL OR status = $1)
		  AND id > $2
		ORDER BY id
		LIMIT $3
	`, filter.Status, filter.AfterID, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()

	bookings := make([]*domain.Booking, 0, filter.Limit)
	for rows.Next() {
		booking, err := scanBooking(rows)
		if err != nil {
			return nil, fmt.Errorf("scan booking: %w", err)
		}
		bookings = append(bookings, booking)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bookings: %w", err)
	}
	return bookings, nil
}

// Update uses the version column for optimistic concurrency control. A stale
// writer gets ErrVersionChanged instead of silently overwriting a newer state.
func (r *Repository) Update(ctx context.Context, booking *domain.Booking, expectedVersion int, event events.Outbox) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		UPDATE bookings
		SET status = $2,
		    cancellation_reason = $3,
		    version = version + 1,
		    updated_at = $4
		WHERE id = $1 AND version = $5
		RETURNING version, updated_at
	`,
		booking.ID,
		booking.Status,
		nullString(booking.CancellationReason),
		booking.UpdatedAt,
		expectedVersion,
	).Scan(&booking.Version, &booking.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if checkErr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bookings WHERE id = $1)`, booking.ID).Scan(&exists); checkErr != nil {
			return fmt.Errorf("check booking existence: %w", checkErr)
		}
		if !exists {
			return app.ErrNotFound
		}
		return app.ErrVersionChanged
	}
	if err != nil {
		return fmt.Errorf("update booking: %w", err)
	}

	// Marshal only after the guarded write succeeds: a stale writer must not
	// fail on (or pay for) event encoding it will never commit.
	payload, err := event.Envelope.Marshal()
	if err != nil {
		return fmt.Errorf("marshal outbox event: %w", err)
	}

	if err := insertOutbox(ctx, tx, event, payload); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit update booking: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, tx pgx.Tx, event events.Outbox, payload []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_messages (event_id, event_type, payload, trace_parent)
		VALUES ($1::uuid, $2, $3, $4)
	`, event.Envelope.ID, event.Envelope.Type, string(payload), nullString(event.TraceParent))
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanBooking(row scanner) (*domain.Booking, error) {
	var (
		booking domain.Booking
		status  string
	)

	err := row.Scan(
		&booking.ID,
		&booking.ServiceID,
		&booking.ServiceName,
		&booking.ServicePriceCents,
		&booking.DurationMinutes,
		&booking.CustomerName,
		&booking.CustomerEmail,
		&booking.ScheduledAt,
		&status,
		&booking.CancellationReason,
		&booking.Version,
		&booking.CreatedAt,
		&booking.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	booking.Status = domain.Status(status)
	booking.ScheduledAt = booking.ScheduledAt.UTC()
	booking.CreatedAt = booking.CreatedAt.UTC()
	booking.UpdatedAt = booking.UpdatedAt.UTC()
	return &booking, nil
}

func nullString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
