package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a new Postgres connection pool.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// READ COMMITTED is the default isolation level for Postgres, which is what
	// we want — every contended read in T2 is an explicit FOR UPDATE, so no
	// higher isolation level is needed (see §8 Concurrency).
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// IsUniqueViolation returns true if the error is a Postgres unique-constraint
// violation (SQLSTATE 23505). This is the structural signal for an idempotency
// key collision (§9 — "Duplicate detection at write time by the unique-constraint
// violation on T1's INSERT, never by a pre-SELECT").
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// IsForeignKeyViolation returns true if the error is a Postgres foreign-key
// violation (SQLSTATE 23503). This is how we detect a transfer that references
// a non-existent wallet — translated to a 404 at the handler.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return false
}

// IsCheckViolation returns true if the error is a Postgres CHECK-constraint
// violation (SQLSTATE 23514). We don't expect to see these in correct operation
// — they would indicate a serious bug (e.g. an app-layer overdraft check was
// bypassed and balance>=0 caught it). Worth distinguishing for logging.
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23514"
	}
	return false
}
