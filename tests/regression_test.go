package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/repository"
	"github.com/wallet-transfer-service/internal/service"
)

// =============================================================================
// Defense-in-depth regression tests
//
// These tests do NOT test the happy-path behavior of the service. They test
// the safety-net constraints that catch bugs in the code above them. Their
// purpose is to fail if someone removes the constraint or weakens the
// enforcement, even if all behavioral tests still pass.
//
// If a regression removes UNIQUE(idempotency_key) but introduces an
// application-level pre-SELECT, the concurrency tests would catch it via
// race conditions. But if a regression removes UNIQUE(transfer_id, direction)
// from the ledger, the concurrency tests probably won't catch it because the
// status check fires first. THAT'S exactly what these tests protect against:
// the case where bug #1 (status check bypassed somehow) plus bug #2 (ledger
// UNIQUE dropped) would compound. Defense in depth requires testing each
// layer in isolation.
// =============================================================================

// -----------------------------------------------------------------------------
// I7 — State machine monotonicity
//
// The conditional UPDATE in TransferRepository.UpdateTerminalConditional must
// affect ZERO rows when called on a transfer that is already in a terminal
// state. Without the `WHERE status='PENDING'` guard, a buggy code path could
// flip a PROCESSED transfer to FAILED (or vice versa).
// -----------------------------------------------------------------------------

func TestRegression_I7_TerminalUpdateOnAlreadyProcessed_IsNoOp(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Drive a transfer to PROCESSED normally.
	tr, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "i7-processed",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, tr.Status)

	// Now directly attempt — via the repository, bypassing the service — to
	// flip it to FAILED. The conditional UPDATE WHERE status='PENDING' must
	// prevent this from taking effect.
	originalProcessedAt := tr.ProcessedAt
	require.NotNil(t, originalProcessedAt)

	// Construct an in-memory transfer with FAILED state. (We can't use the
	// domain entity's MarkFailed because it rejects non-PENDING states by
	// design — which is itself part of I7 enforcement at the domain layer.
	// We build the entity manually to simulate a buggy code path that bypasses
	// the domain entity and goes straight to the repository.)
	manualFailReason := "INJECTED_BY_REGRESSION_TEST"
	faked := &domain.Transfer{
		ID:             tr.ID,
		IdempotencyKey: tr.IdempotencyKey,
		PayloadHash:    tr.PayloadHash,
		FromWalletID:   tr.FromWalletID,
		ToWalletID:     tr.ToWalletID,
		Amount:         tr.Amount,
		Status:         domain.StatusFailed,
		FailureReason:  &manualFailReason,
	}
	now := time.Now()
	faked.ProcessedAt = &now

	repo := repository.NewTransferRepository()
	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	rows, err := repo.UpdateTerminalConditional(ctx, tx, faked)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// CRITICAL ASSERTION: zero rows affected. The WHERE clause filtered it out.
	assert.Equal(t, int64(0), rows,
		"I7 violation: conditional UPDATE allowed a terminal-state transfer to be re-terminalized")

	// Verify the row is unchanged by reading it back.
	tx2, _ := env.pool.Begin(ctx)
	defer tx2.Rollback(ctx)
	after, err := repo.GetByIDForUpdate(ctx, tx2, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, after.Status,
		"I7 violation: PROCESSED transfer was mutated to FAILED")
	assert.Nil(t, after.FailureReason,
		"I7 violation: failure_reason was set on a PROCESSED transfer")
}

func TestRegression_I7_TerminalUpdateOnAlreadyFailed_IsNoOp(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 50) // insufficient for the 100-transfer
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Drive a transfer to FAILED via insufficient funds.
	tr, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "i7-failed",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, tr.Status)

	// Attempt to flip it to PROCESSED. Must be a no-op.
	processedAt := time.Now()
	faked := &domain.Transfer{
		ID:             tr.ID,
		IdempotencyKey: tr.IdempotencyKey,
		PayloadHash:    tr.PayloadHash,
		FromWalletID:   tr.FromWalletID,
		ToWalletID:     tr.ToWalletID,
		Amount:         tr.Amount,
		Status:         domain.StatusProcessed,
		ProcessedAt:    &processedAt,
	}

	repo := repository.NewTransferRepository()
	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	rows, err := repo.UpdateTerminalConditional(ctx, tx, faked)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, int64(0), rows,
		"I7 violation: conditional UPDATE allowed FAILED to be flipped to PROCESSED")

	// Re-read and verify.
	tx2, _ := env.pool.Begin(ctx)
	defer tx2.Rollback(ctx)
	after, err := repo.GetByIDForUpdate(ctx, tx2, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, after.Status,
		"I7 violation: FAILED transfer was mutated to PROCESSED")

	// And critically — no money moved, no ledger entries created by the failed transition.
	assert.Equal(t, int64(50), env.walletBalance("w1"))
	assert.Equal(t, int64(0), env.walletBalance("w2"))
	assert.Equal(t, 0, env.countLedgerEntries(),
		"I7 violation: ledger entries appeared for a transfer that should have stayed FAILED")
}

// -----------------------------------------------------------------------------
// Ledger UNIQUE(transfer_id, direction) — executor-level idempotency
//
// This constraint catches double-posting at the DB level even if the
// application-layer status check is bypassed or buggy. It is the absolute
// backstop on duplicate side effects.
// -----------------------------------------------------------------------------

func TestRegression_LedgerUniqueDirection_PreventsDoubleDebit(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Drive a successful transfer; ledger now has one DEBIT and one CREDIT.
	tr, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "ledger-uniq-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, tr.Status)
	require.Equal(t, 2, env.countLedgerEntries())

	// Attempt to insert a SECOND DEBIT for the same transfer_id.
	// This simulates a hypothetical bug where T2 ran twice on the same row
	// AND the status check failed to bail. The UNIQUE constraint is the
	// last line of defense.
	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (transfer_id, wallet_id, direction, amount, balance_after)
		VALUES ($1, $2, 'DEBIT', $3, $4)
	`, tr.ID, tr.FromWalletID, tr.Amount, int64(0))

	// CRITICAL ASSERTION: the DB must reject this with a unique violation.
	require.Error(t, err, "ledger UNIQUE(transfer_id, direction) constraint missing or weakened")
	assert.True(t, repository.IsUniqueViolation(err),
		"expected unique-violation (23505), got: %v", err)

	// Verify the ledger is unchanged (the failed INSERT was rolled back).
	_ = tx.Rollback(ctx)
	assert.Equal(t, 2, env.countLedgerEntries(),
		"ledger entries grew despite the constraint violation — rollback failed?")
}

func TestRegression_LedgerUniqueDirection_PreventsDoubleCredit(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	tr, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "ledger-uniq-2",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, tr.Status)

	// Attempt a duplicate CREDIT — same symmetry, just on the other side.
	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (transfer_id, wallet_id, direction, amount, balance_after)
		VALUES ($1, $2, 'CREDIT', $3, $4)
	`, tr.ID, tr.ToWalletID, tr.Amount, int64(200))

	require.Error(t, err)
	assert.True(t, repository.IsUniqueViolation(err))

	_ = tx.Rollback(ctx)
	assert.Equal(t, 2, env.countLedgerEntries())
}

// TestRegression_LedgerUniqueDirection_AllowsDifferentTransferSameDirection
// confirms the constraint is on (transfer_id, direction), NOT on direction
// alone. A different transfer's DEBIT must be insertable — otherwise the
// constraint would be wrong in the other direction.
func TestRegression_LedgerUniqueDirection_AllowsDifferentTransferSameDirection(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 1000)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Two successful transfers from w1 — both DEBIT w1.
	_, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "uniq-allow-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)

	_, err = env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "uniq-allow-2",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)

	// Both succeed: 4 ledger entries total (2 per transfer). The constraint
	// scoping is (transfer_id, direction), so different transfer IDs can both
	// have a DEBIT on the same wallet.
	assert.Equal(t, 4, env.countLedgerEntries())
}

// -----------------------------------------------------------------------------
// Bonus: UNIQUE(transfer_id, wallet_id) — also part of defense in depth
//
// This second unique constraint says a transfer touches each wallet at most
// once. With direction-based UNIQUE alone, you could in theory have a buggy
// CREDIT entry on the source wallet (a third entry beyond the legitimate
// DEBIT). The (transfer_id, wallet_id) constraint catches that too.
// -----------------------------------------------------------------------------

func TestRegression_LedgerUniqueWallet_PreventsThirdEntryOnSameWallet(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	tr, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "wallet-uniq-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, tr.Status)

	// Attempt to add a CREDIT to w1 (the source wallet) for this same transfer.
	// w1 already has a DEBIT entry from this transfer; adding any second entry
	// for w1 should fail on UNIQUE(transfer_id, wallet_id).
	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (transfer_id, wallet_id, direction, amount, balance_after)
		VALUES ($1, 'w1', 'CREDIT', $2, $3)
	`, tr.ID, tr.Amount, int64(500))

	require.Error(t, err)
	assert.True(t, repository.IsUniqueViolation(err),
		"UNIQUE(transfer_id, wallet_id) missing — a transfer can hit the same wallet twice")
}

// -----------------------------------------------------------------------------
// Idempotency key UNIQUE — the top-level gate
//
// This is exercised indirectly by every concurrency idempotency test, but a
// direct test gives a sharper failure signal if the constraint is dropped.
// -----------------------------------------------------------------------------

func TestRegression_TransferIdempotencyKeyUnique_PreventsDuplicateRows(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Insert one transfer normally via the service.
	_, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "uniq-key-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)

	// Now attempt to insert a SECOND transfer row with the same idempotency
	// key, directly via the repository — bypassing the service's REPLAY routing
	// to test the DB-level guarantee.
	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	dup, err := domain.NewTransfer(
		uuid.New(), // different transfer ID
		"uniq-key-1",
		"unrelated_hash",
		"w1", "w2", 200,
	)
	require.NoError(t, err)

	repo := repository.NewTransferRepository()
	err = repo.Insert(ctx, tx, dup)

	require.Error(t, err)
	assert.True(t, repository.IsUniqueViolation(err),
		"UNIQUE(idempotency_key) missing — duplicate transfer rows can exist for the same key")

	// Verify only one row exists.
	_ = tx.Rollback(ctx)
	assert.Equal(t, 1, env.countTransfers())
}
