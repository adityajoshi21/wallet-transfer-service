package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wallet-transfer-service/internal/domain"
)

// TransferRepository persists and retrieves Transfer entities.
//
// IMPORTANT: every method that participates in a transaction accepts a
// pgx.Tx from the calling service layer, never opens its own. The service
// layer owns transaction boundaries — that's the architectural rule from §14.
type TransferRepository struct{}

func NewTransferRepository() *TransferRepository {
	return &TransferRepository{}
}

// If the idempotency_key is already taken, returns a Postgres unique-violation
// error which the caller detects via IsUniqueViolation.
//
// If a referenced wallet does not exist, returns a FK violation which the
// caller detects via IsForeignKeyViolation.
func (r *TransferRepository) Insert(ctx context.Context, tx pgx.Tx, t *domain.Transfer) error {
	const q = `
		INSERT INTO transfers (
			id, idempotency_key, payload_hash,
			from_wallet_id, to_wallet_id, amount, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	_, err := tx.Exec(ctx, q,
		t.ID, t.IdempotencyKey, t.PayloadHash,
		t.FromWalletID, t.ToWalletID, t.Amount, string(t.Status),
	)
	return err
}

// GetByIDForUpdate fetches a transfer by ID and acquires a row-level lock on it
// (FOR UPDATE). Used as the entry to T2 (§7).
func (r *TransferRepository) GetByIDForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*domain.Transfer, error) {
	const q = `
		SELECT id, idempotency_key, payload_hash,
		       from_wallet_id, to_wallet_id, amount,
		       status, failure_reason, created_at, processed_at
		FROM transfers
		WHERE id = $1
		FOR UPDATE
	`
	return r.scanOne(ctx, tx, q, id)
}

// GetByKeyForUpdate fetches a transfer by idempotency_key and acquires a
// row-level lock. Used by REPLAY (§7) — the lock is what allows the retry
// handler to inline-execute T2 on a PENDING row safely.
func (r *TransferRepository) GetByKeyForUpdate(ctx context.Context, tx pgx.Tx, key string) (*domain.Transfer, error) {
	const q = `
		SELECT id, idempotency_key, payload_hash,
		       from_wallet_id, to_wallet_id, amount,
		       status, failure_reason, created_at, processed_at
		FROM transfers
		WHERE idempotency_key = $1
		FOR UPDATE
	`
	return r.scanOne(ctx, tx, q, key)
}

// UpdateTerminalConditional transitions a transfer to a terminal state
// (PROCESSED or FAILED) ONLY if it is currently PENDING.
//
// Returns the number of rows affected. If 0, the caller treats it as a
// state-machine anomaly (someone else already terminated this transfer) and
// routes the request to REPLAY. This is the DB-level enforcement of I7 (state
// monotonicity) — see §3.
func (r *TransferRepository) UpdateTerminalConditional(ctx context.Context, tx pgx.Tx, t *domain.Transfer) (int64, error) {
	if !t.IsTerminal() {
		return 0, fmt.Errorf("UpdateTerminalConditional called with non-terminal status %q", t.Status)
	}
	const q = `
		UPDATE transfers
		   SET status = $2,
		       failure_reason = $3,
		       processed_at = $4
		 WHERE id = $1
		   AND status = 'PENDING'
	`
	res, err := tx.Exec(ctx, q, t.ID, string(t.Status), t.FailureReason, t.ProcessedAt)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// ListStalePending returns up to `limit` transfers that are PENDING and older
// than the given threshold, locking them with FOR UPDATE SKIP LOCKED so multiple
// recovery workers do not collide. Used by the recovery worker (§13).
func (r *TransferRepository) ListStalePending(ctx context.Context, tx pgx.Tx, olderThanSeconds int, limit int) ([]*domain.Transfer, error) {
	q := fmt.Sprintf(`
		SELECT id, idempotency_key, payload_hash,
		       from_wallet_id, to_wallet_id, amount,
		       status, failure_reason, created_at, processed_at
		FROM transfers
		WHERE status = 'PENDING'
		  AND created_at < now() - interval '%d seconds'
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, olderThanSeconds)
	rows, err := tx.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Transfer
	for rows.Next() {
		t, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListByWallet returns up to `limit` most recent transfers involving the given
// wallet (as either source or destination), ordered by created_at DESC.
// Used by the GET /wallets/{id}/transfers endpoint.
//
// Pagination uses keyset (created_at, id) — `before` is the created_at of the
// last transfer in the previous page; pass zero time for the first page.
func (r *TransferRepository) ListByWallet(ctx context.Context, tx pgx.Tx, walletID string, before time.Time, limit int) ([]*domain.Transfer, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if before.IsZero() {
		const q = `
			SELECT id, idempotency_key, payload_hash,
			       from_wallet_id, to_wallet_id, amount,
			       status, failure_reason, created_at, processed_at
			FROM transfers
			WHERE from_wallet_id = $1 OR to_wallet_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2
		`
		rows, err = tx.Query(ctx, q, walletID, limit)
	} else {
		const q = `
			SELECT id, idempotency_key, payload_hash,
			       from_wallet_id, to_wallet_id, amount,
			       status, failure_reason, created_at, processed_at
			FROM transfers
			WHERE (from_wallet_id = $1 OR to_wallet_id = $1)
			  AND created_at < $2
			ORDER BY created_at DESC, id DESC
			LIMIT $3
		`
		rows, err = tx.Query(ctx, q, walletID, before, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Transfer
	for rows.Next() {
		t, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanOne runs a single-row query and scans into a Transfer.
func (r *TransferRepository) scanOne(ctx context.Context, tx pgx.Tx, q string, args ...any) (*domain.Transfer, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, domain.ErrTransferNotFound
	}
	return r.scanRow(rows)
}

func (r *TransferRepository) scanRow(rows pgx.Rows) (*domain.Transfer, error) {
	var t domain.Transfer
	var status string
	if err := rows.Scan(
		&t.ID, &t.IdempotencyKey, &t.PayloadHash,
		&t.FromWalletID, &t.ToWalletID, &t.Amount,
		&status, &t.FailureReason, &t.CreatedAt, &t.ProcessedAt,
	); err != nil {
		return nil, fmt.Errorf("scan transfer: %w", err)
	}
	t.Status = domain.TransferStatus(status)
	return &t, nil
}
