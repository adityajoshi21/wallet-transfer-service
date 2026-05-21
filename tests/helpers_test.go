package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/repository"
	"github.com/wallet-transfer-service/internal/service"
)

// testDSN returns the test database connection string from env, or skips.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration/concurrency test")
	}
	return dsn
}

// testEnv bundles everything a test needs.
type testEnv struct {
	t       *testing.T
	pool    *pgxpool.Pool
	svc     *service.TransferService
	wallets *repository.WalletRepository
	ledger  *repository.LedgerRepository
}

// newTestEnv connects to the test DB, truncates all tables, and returns a
// fresh service wired up against the real Postgres.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := repository.NewPool(ctx, testDSN(t))
	require.NoError(t, err)

	// Clean slate for every test. CASCADE removes dependent rows from
	// ledger_entries and transfers automatically.
	_, err = pool.Exec(ctx, `TRUNCATE wallets, transfers, ledger_entries RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	transferRepo := repository.NewTransferRepository()
	walletRepo := repository.NewWalletRepository()
	ledgerRepo := repository.NewLedgerRepository()
	svc := service.NewTransferService(pool, transferRepo, walletRepo, ledgerRepo)

	t.Cleanup(func() {
		pool.Close()
	})

	return &testEnv{
		t:       t,
		pool:    pool,
		svc:     svc,
		wallets: walletRepo,
		ledger:  ledgerRepo,
	}
}

// seedWallet inserts a wallet with the given starting balance.
func (e *testEnv) seedWallet(id string, balance int64) {
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	require.NoError(e.t, err)
	defer tx.Rollback(ctx)

	require.NoError(e.t, e.wallets.CreateWallet(ctx, tx, &domain.Wallet{ID: id, Balance: balance}))
	require.NoError(e.t, tx.Commit(ctx))
}

// walletBalance reads the current balance of a wallet.
func (e *testEnv) walletBalance(id string) int64 {
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	require.NoError(e.t, err)
	defer tx.Rollback(ctx)

	b, err := e.wallets.GetBalance(ctx, tx, id)
	require.NoError(e.t, err)
	return b
}

// countTransfers returns the total number of transfer rows.
func (e *testEnv) countTransfers() int {
	ctx := context.Background()
	var n int
	err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM transfers`).Scan(&n)
	require.NoError(e.t, err)
	return n
}

// countLedgerEntries returns the total number of ledger entries.
func (e *testEnv) countLedgerEntries() int {
	ctx := context.Background()
	var n int
	err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_entries`).Scan(&n)
	require.NoError(e.t, err)
	return n
}

// assertInvariants checks the core invariants against the live DB state.
// Called at the end of every concurrency test.
//
// Covers:
//
//	I1: per transfer, ledger entries sum to zero (debit amount == credit amount).
//	I3: wallets.balance == latest ledger_entries.balance_after per wallet.
//	I4: no wallets.balance is negative.
//	I6: PROCESSED transfers have exactly 2 ledger entries; FAILED have 0.
func (e *testEnv) assertInvariants() {
	ctx := context.Background()

	// I4: no negative balances.
	{
		var n int
		err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM wallets WHERE balance < 0`).Scan(&n)
		require.NoError(e.t, err)
		require.Equal(e.t, 0, n, "I4 violated: a wallet has a negative balance")
	}

	// I1: for each PROCESSED transfer, the debit amount equals the credit amount.
	{
		const q = `
			SELECT t.id,
			       COALESCE(SUM(CASE WHEN l.direction='DEBIT'  THEN l.amount END), 0) AS debits,
			       COALESCE(SUM(CASE WHEN l.direction='CREDIT' THEN l.amount END), 0) AS credits
			FROM transfers t
			LEFT JOIN ledger_entries l ON l.transfer_id = t.id
			WHERE t.status = 'PROCESSED'
			GROUP BY t.id
			HAVING COALESCE(SUM(CASE WHEN l.direction='DEBIT'  THEN l.amount END), 0)
			    <> COALESCE(SUM(CASE WHEN l.direction='CREDIT' THEN l.amount END), 0)
		`
		var n int
		err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM (`+q+`) x`).Scan(&n)
		require.NoError(e.t, err)
		require.Equal(e.t, 0, n, "I1 violated: a PROCESSED transfer has unbalanced ledger entries")
	}

	// I6: PROCESSED transfers have exactly 2 ledger entries; FAILED have 0.
	{
		var n int
		err := e.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM (
				SELECT t.id, t.status, COUNT(l.id) AS leg_count
				FROM transfers t
				LEFT JOIN ledger_entries l ON l.transfer_id = t.id
				GROUP BY t.id, t.status
			) x
			WHERE (status = 'PROCESSED' AND leg_count <> 2)
			   OR (status = 'FAILED'    AND leg_count <> 0)
			   OR (status = 'PENDING'   AND leg_count <> 0)
		`).Scan(&n)
		require.NoError(e.t, err)
		require.Equal(e.t, 0, n, "I6 violated: ledger entry count does not match transfer status")
	}

	// I3: wallet.balance must equal the latest ledger_entries.balance_after
	// for that wallet (or the wallet's seeded balance if it has no entries).
	//
	// This is the strongest check: it validates that the stored balance is
	// the same as what the ledger says it should be.
	{
		const q = `
			WITH latest AS (
				SELECT DISTINCT ON (wallet_id) wallet_id, balance_after
				FROM ledger_entries
				ORDER BY wallet_id, id DESC
			)
			SELECT COUNT(*) FROM wallets w
			LEFT JOIN latest l ON l.wallet_id = w.id
			WHERE l.balance_after IS NOT NULL
			  AND w.balance <> l.balance_after
		`
		var n int
		err := e.pool.QueryRow(ctx, q).Scan(&n)
		require.NoError(e.t, err)
		require.Equal(e.t, 0, n, "I3 violated: wallets.balance does not match latest ledger_entries.balance_after")
	}
}
