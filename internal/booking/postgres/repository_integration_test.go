//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	bookingapp "github.com/xshubhamg/chore-microservices/internal/booking/app"
	"github.com/xshubhamg/chore-microservices/internal/booking/domain"
	bookingpostgres "github.com/xshubhamg/chore-microservices/internal/booking/postgres"
	"github.com/xshubhamg/chore-microservices/internal/platform/config"
	"github.com/xshubhamg/chore-microservices/internal/platform/events"
	"github.com/xshubhamg/chore-microservices/internal/platform/migrate"
	"github.com/xshubhamg/chore-microservices/internal/platform/postgres"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; run `make test-integration`")
	}

	ctx := context.Background()
	cfg := config.DBConfig{
		DSN:             dsn,
		MaxConns:        5,
		MinConns:        0,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: time.Minute,
		ConnectTimeout:  20 * time.Second,
	}

	pool, err := postgres.Connect(ctx, cfg, "booking-integration-test", testLogger())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := migrate.Up(ctx, dsn, bookingpostgres.Migrations(), "migrations", testLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE bookings, outbox_messages, idempotency_keys RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func newBooking(t *testing.T) *domain.Booking {
	t.Helper()

	now := time.Now().UTC()
	booking, err := domain.New(domain.NewInput{
		ServiceID:         1,
		ServiceName:       "Deep clean",
		ServicePriceCents: 12900,
		DurationMinutes:   180,
		CustomerName:      "Ada Lovelace",
		CustomerEmail:     "ada@example.com",
		ScheduledAt:       now.Add(48 * time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("build booking: %v", err)
	}
	return booking
}

func TestCreateWritesBookingAndOutboxAtomically(t *testing.T) {
	pool := testPool(t)
	repo := bookingpostgres.New(pool)

	booking := newBooking(t)
	err := repo.Create(context.Background(), booking, func() (events.Outbox, error) {
		envelope, err := events.New(events.TypeBookingCreated, events.ProducerBooking, events.BookingPayload{
			BookingID: booking.ID,
			ServiceID: booking.ServiceID,
			Email:     booking.CustomerEmail,
			Status:    string(booking.Status),
		})
		return events.Outbox{Envelope: envelope}, err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if booking.ID == 0 {
		t.Fatal("booking ID was not assigned")
	}

	var (
		bookings int
		outbox   int
	)
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings`).Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_messages`).Scan(&outbox); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if bookings != 1 || outbox != 1 {
		t.Fatalf("bookings=%d outbox=%d, want 1 and 1", bookings, outbox)
	}

	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_messages LIMIT 1`).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}

	envelope, err := events.Parse(payload)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	var eventPayload events.BookingPayload
	if err := json.Unmarshal(envelope.Payload, &eventPayload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if eventPayload.BookingID != booking.ID {
		t.Errorf("event booking_id = %d, want %d", eventPayload.BookingID, booking.ID)
	}
}

func TestUpdateUsesOptimisticLocking(t *testing.T) {
	pool := testPool(t)
	repo := bookingpostgres.New(pool)

	booking := newBooking(t)
	if err := repo.Create(context.Background(), booking, func() (events.Outbox, error) {
		envelope, err := events.New(events.TypeBookingCreated, events.ProducerBooking, events.BookingPayload{BookingID: booking.ID})
		return events.Outbox{Envelope: envelope}, err
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	stored, err := repo.Get(context.Background(), booking.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Version != 1 {
		t.Fatalf("version = %d, want 1", stored.Version)
	}

	// Stale writer: the aggregate has already moved to version 1 and the repo
	// is told it is still at version 0.
	stale := *stored
	stale.Status = domain.StatusCancelled
	stale.UpdatedAt = time.Now().UTC()
	err = repo.Update(context.Background(), &stale, 0, events.Outbox{})
	if err != bookingapp.ErrVersionChanged {
		t.Fatalf("err = %v, want ErrVersionChanged", err)
	}

	var outboxCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_messages`).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox rows = %d, a failed update must not append an event", outboxCount)
	}

	// Correct writer.
	current := *stored
	current.Status = domain.StatusConfirmed
	current.UpdatedAt = time.Now().UTC()
	envelope, _ := events.New(events.TypeBookingConfirmed, events.ProducerBooking, events.BookingPayload{BookingID: current.ID})
	if err := repo.Update(context.Background(), &current, stored.Version, events.Outbox{Envelope: envelope}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if current.Version != 2 {
		t.Errorf("version = %d, want 2", current.Version)
	}

	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_messages`).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 2 {
		t.Fatalf("outbox rows = %d, want 2", outboxCount)
	}
}

func TestOutboxClaimAndPublishLifecycle(t *testing.T) {
	pool := testPool(t)
	repo := bookingpostgres.New(pool)
	store := bookingpostgres.NewOutboxStore(pool)

	booking := newBooking(t)
	if err := repo.Create(context.Background(), booking, func() (events.Outbox, error) {
		envelope, err := events.New(events.TypeBookingCreated, events.ProducerBooking, events.BookingPayload{BookingID: booking.ID})
		return events.Outbox{Envelope: envelope}, err
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	messages, err := store.Claim(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("claimed %d messages, want 1", len(messages))
	}
	if messages[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", messages[0].Attempts)
	}

	// A second relay must not see the leased message.
	again, err := store.Claim(context.Background(), 10, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second claim returned %d messages, want 0 (leased)", len(again))
	}

	if err := store.MarkPublished(context.Background(), messages[0].ID); err != nil {
		t.Fatalf("mark published: %v", err)
	}

	pending, _, err := store.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
}
