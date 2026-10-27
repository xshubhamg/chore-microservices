// Package idempotency implements the Idempotency-Key pattern for unsafe HTTP
// methods. A client retry with the same key returns the original response
// instead of creating a second booking.
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrKeyReuse = errors.New("idempotency key reused with a different request body")

type State string

const (
	StateNew        State = "new"
	StateInProgress State = "in_progress"
	StateCompleted  State = "completed"
)

type Record struct {
	StatusCode int
	Body       []byte
}

type Claim struct {
	State  State
	Record *Record
}

type Store interface {
	// Claim reserves the key for this request. ErrKeyReuse is returned when the
	// key already exists with a different request hash.
	Claim(ctx context.Context, key, requestHash string, ttl time.Duration) (Claim, error)
	Complete(ctx context.Context, key string, record Record) error
	Release(ctx context.Context, key string) error
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Claim(ctx context.Context, key, requestHash string, ttl time.Duration) (Claim, error) {
	var claimedKey string

	// Insert a new claim, or take over one whose TTL elapsed (the original
	// request either crashed or never completed).
	err := s.pool.QueryRow(ctx, `
		INSERT INTO idempotency_keys (key, request_hash, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (key) DO UPDATE
		SET request_hash = EXCLUDED.request_hash,
		    state = 'in_progress',
		    response_status = NULL,
		    response_body = NULL,
		    created_at = now(),
		    completed_at = NULL,
		    expires_at = EXCLUDED.expires_at
		WHERE idempotency_keys.expires_at < now()
		RETURNING key
	`, key, requestHash, ttl.String()).Scan(&claimedKey)
	if err == nil {
		return Claim{State: StateNew}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, fmt.Errorf("claim idempotency key: %w", err)
	}

	var (
		storedHash string
		state      State
		status     *int
		body       []byte
	)
	err = s.pool.QueryRow(ctx, `
		SELECT request_hash, state, response_status, response_body
		FROM idempotency_keys
		WHERE key = $1
	`, key).Scan(&storedHash, &state, &status, &body)
	if err != nil {
		return Claim{}, fmt.Errorf("load idempotency key: %w", err)
	}

	if storedHash != requestHash {
		return Claim{}, ErrKeyReuse
	}

	switch state {
	case StateCompleted:
		if status == nil {
			return Claim{}, errors.New("completed idempotency record without response")
		}
		return Claim{State: StateCompleted, Record: &Record{StatusCode: *status, Body: body}}, nil
	default:
		return Claim{State: StateInProgress}, nil
	}
}

func (s *PostgresStore) Complete(ctx context.Context, key string, record Record) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET state = 'completed',
		    response_status = $2,
		    response_body = $3,
		    completed_at = now()
		WHERE key = $1
	`, key, record.StatusCode, string(record.Body))
	return err
}

func (s *PostgresStore) Release(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE key = $1`, key)
	return err
}

// PurgeLoop deletes expired keys. It is meant to run as a supervised actor.
func (s *PostgresStore) PurgeLoop(ctx context.Context, interval time.Duration, log *slog.Logger) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
			if err != nil {
				log.Warn("purge idempotency keys failed", slog.String("error", err.Error()))
				continue
			}
			if tag.RowsAffected() > 0 {
				log.Info("purged idempotency keys", slog.Int64("rows", tag.RowsAffected()))
			}
		}
	}
}
