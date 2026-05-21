package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wallet-transfer-service/internal/domain"
)

// LedgerRepository persists ledger entries. Append-only — no UPDATE or DELETE
// methods exist by design (§12). Corrections are new compensating entries.
type LedgerRepository struct{}

func NewLedgerRepository() *LedgerRepository {
	return &LedgerRepository{}
}

// InsertPair inserts both legs of a double-entry pair in a single statement.
// The DB enforces I1 structurally via UNIQUE(transfer_id, direction) —
// if a retry somehow attempts to post the same pair again, the second
// INSERT will fail with a unique-violation, which the service treats as a
// signal that the ledger has already been written (recovery worker path).
func (r *LedgerRepository) InsertPair(ctx context.Context, tx pgx.Tx, debit, credit domain.LedgerEntry) error {
	const q = `
		INSERT INTO ledger_entries
			(transfer_id, wallet_id, direction, amount, balance_after)
		VALUES
			($1, $2, $3, $4, $5),
			($6, $7, $8, $9, $10)
	`
	_, err := tx.Exec(ctx, q,
		debit.TransferID, debit.WalletID, string(debit.Direction), debit.Amount, debit.BalanceAfter,
		credit.TransferID, credit.WalletID, string(credit.Direction), credit.Amount, credit.BalanceAfter,
	)
	return err
}

// ListByTransfer returns the ledger entries for a given transfer in insertion
// order. Used in tests and reconciliation; not in the hot path of T2.
func (r *LedgerRepository) ListByTransfer(ctx context.Context, tx pgx.Tx, transferID uuid.UUID) ([]domain.LedgerEntry, error) {
	const q = `
		SELECT id, transfer_id, wallet_id, direction, amount, balance_after, created_at
		FROM ledger_entries
		WHERE transfer_id = $1
		ORDER BY id
	`
	rows, err := tx.Query(ctx, q, transferID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.LedgerEntry
	for rows.Next() {
		var e domain.LedgerEntry
		var dir string
		if err := rows.Scan(&e.ID, &e.TransferID, &e.WalletID, &dir, &e.Amount, &e.BalanceAfter, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		e.Direction = domain.EntryDirection(dir)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountByTransfer returns the number of ledger entries for a transfer.
// Used in tests to assert exactly two entries (I6) per PROCESSED transfer
// and zero per FAILED.
func (r *LedgerRepository) CountByTransfer(ctx context.Context, tx pgx.Tx, transferID uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM ledger_entries WHERE transfer_id = $1`
	var n int
	err := tx.QueryRow(ctx, q, transferID).Scan(&n)
	return n, err
}
