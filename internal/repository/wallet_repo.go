package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/wallet-transfer-service/internal/domain"
)

// WalletRepository persists and retrieves Wallet entities.
type WalletRepository struct{}

func NewWalletRepository() *WalletRepository {
	return &WalletRepository{}
}

// LockForUpdate fetches the wallets with the given IDs and acquires row-level
// locks on each, in ASCENDING ID ORDER. This is the deterministic lock order
// that eliminates Race 3 (deadlock from crossing A→B / B→A transfers — see §8).
//
// The sort happens here, not in the caller, to make the locking invariant
// impossible to break by accident.
//
// Returns wallets in the same sorted order. The service layer then picks out
// `from` and `to` by ID.
func (r *WalletRepository) LockForUpdate(ctx context.Context, tx pgx.Tx, ids []string) ([]*domain.Wallet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)

	const q = `
		SELECT id, balance, created_at, updated_at
		FROM wallets
		WHERE id = ANY($1)
		ORDER BY id ASC
		FOR UPDATE			-- acquire row-level locks on the returned wallets
	`
	rows, err := tx.Query(ctx, q, sorted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Wallet
	for rows.Next() {
		var w domain.Wallet
		if err := rows.Scan(&w.ID, &w.Balance, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan wallet: %w", err)
		}
		out = append(out, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(sorted) {
		return nil, domain.ErrWalletNotFound
	}
	return out, nil
}

// GetByID returns the full wallet record for a given ID. No locking; used by
// the GET /wallets/{id} read endpoint.
func (r *WalletRepository) GetByID(ctx context.Context, tx pgx.Tx, id string) (*domain.Wallet, error) {
	const q = `SELECT id, balance, created_at, updated_at FROM wallets WHERE id = $1`
	var w domain.Wallet
	err := tx.QueryRow(ctx, q, id).Scan(&w.ID, &w.Balance, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWalletNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// GetBalance returns the current balance for a wallet. No locking; used for
// read-only balance queries (out-of-scope optional endpoint).
func (r *WalletRepository) GetBalance(ctx context.Context, tx pgx.Tx, id string) (int64, error) {
	const q = `SELECT balance FROM wallets WHERE id = $1`
	var balance int64
	err := tx.QueryRow(ctx, q, id).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrWalletNotFound
	}
	return balance, err
}

// UpdateBalance writes the given balance to the wallet. Assumes the caller
// already holds a FOR UPDATE lock on the row (which is the case in T2).
func (r *WalletRepository) UpdateBalance(ctx context.Context, tx pgx.Tx, w *domain.Wallet) error {
	const q = `
		UPDATE wallets
		   SET balance = $2,
		       updated_at = now()
		 WHERE id = $1
	`
	_, err := tx.Exec(ctx, q, w.ID, w.Balance)
	return err
}

// CreateWallet inserts a wallet row. Used by test fixtures; no public endpoint(out of scope).
func (r *WalletRepository) CreateWallet(ctx context.Context, tx pgx.Tx, w *domain.Wallet) error {
	const q = `
		INSERT INTO wallets (id, balance) VALUES ($1, $2)
	`
	_, err := tx.Exec(ctx, q, w.ID, w.Balance)
	return err
}
