package tests

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

// =============================================================================
// I4: Overdraft prevention under concurrent contention
//
// Source wallet has balance 500. 6 parallel transfers of 100 each.
// Expected: exactly 5 succeed (PROCESSED), exactly 1 fail (FAILED with
// INSUFFICIENT_FUNDS), final balance = 0, destination balance = 500.
//
// This is the test from §16 of the design doc — the canonical proof that the
// FOR UPDATE locking strategy actually prevents double-spend under contention.

func TestConcurrency_NoOverdraftUnderContention(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("source", 500)
	env.seedWallet("dest", 0)

	const numTransfers = 6
	const transferAmount = 100

	var wg sync.WaitGroup
	results := make([]*domain.Transfer, numTransfers)
	errs := make([]error, numTransfers)

	for i := 0; i < numTransfers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tr, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
				IdempotencyKey: uuid.New().String(),
				FromWalletID:   "source",
				ToWalletID:     "dest",
				Amount:         transferAmount,
			})
			results[idx] = tr
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// No system errors should have occurred.
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d: unexpected error", i)
	}

	// Count outcomes.
	var processed, failed int
	for _, tr := range results {
		require.NotNil(t, tr)
		switch tr.Status {
		case domain.StatusProcessed:
			processed++
		case domain.StatusFailed:
			failed++
			require.NotNil(t, tr.FailureReason)
			assert.Equal(t, domain.FailureReasonInsufficientFunds, *tr.FailureReason)
		default:
			t.Fatalf("unexpected status %q", tr.Status)
		}
	}

	// EXACTLY 5 succeed, EXACTLY 1 fail. No overdraft, no lost update.
	assert.Equal(t, 5, processed, "expected exactly 5 PROCESSED transfers")
	assert.Equal(t, 1, failed, "expected exactly 1 FAILED transfers")

	// Final balances reflect only the 5 successful transfers.
	assert.Equal(t, int64(0), env.walletBalance("source"))
	assert.Equal(t, int64(500), env.walletBalance("dest"))

	// 6 transfer rows total (5 PROCESSED + 1 FAILED), 10 ledger entries (5 pairs).
	assert.Equal(t, 6, env.countTransfers())
	assert.Equal(t, 10, env.countLedgerEntries())

	env.assertInvariants()
}

// =============================================================================
// I8: Idempotency race
//
// 50 parallel calls with the SAME idempotency key AND SAME body.
// Expected: exactly ONE transfer record, exactly ONE ledger pair, all 50
// responses identical (same transfer ID, same status). Money moves once.
//
// This proves that the UNIQUE(idempotency_key) constraint serializes T1
// correctly and REPLAY returns the original outcome.
// =============================================================================

func TestConcurrency_IdempotencyRace_SameKey(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 10000)
	env.seedWallet("w2", 0)

	const numCallers = 50
	const sharedKey = "the-one-key"
	const amount = 100

	var wg sync.WaitGroup
	results := make([]*domain.Transfer, numCallers)
	errs := make([]error, numCallers)

	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tr, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
				IdempotencyKey: sharedKey,
				FromWalletID:   "w1",
				ToWalletID:     "w2",
				Amount:         amount,
			})
			results[idx] = tr
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// No errors.
	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
	}

	// All 50 callers received the SAME transfer ID.
	first := results[0]
	require.NotNil(t, first)
	assert.Equal(t, domain.StatusProcessed, first.Status)

	for i, tr := range results {
		require.NotNil(t, tr, "caller %d nil", i)
		assert.Equal(t, first.ID, tr.ID, "caller %d returned a different transfer", i)
		assert.Equal(t, first.Status, tr.Status, "caller %d returned a different status", i)
	}

	// Money moved exactly ONCE.
	assert.Equal(t, int64(9900), env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.walletBalance("w2"))

	// Exactly ONE transfer row and ONE ledger pair in the database.
	assert.Equal(t, 1, env.countTransfers(), "duplicate requests must not create duplicate transfers")
	assert.Equal(t, 2, env.countLedgerEntries(), "duplicate requests must not create duplicate ledger entries")

	env.assertInvariants()
}

// =============================================================================
// Idempotency race with different payloads:
// 50 parallel calls — half with payload A, half with payload B, same key.
// Expected: one of them creates the transfer; the other 49 either return the
// same transfer (if their payload matches) or get ErrIdempotencyKeyReuse.
// =============================================================================

func TestConcurrency_IdempotencyRace_MixedPayloads(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 10000)
	env.seedWallet("w2", 0)
	env.seedWallet("w3", 0)

	const numCallers = 50
	const sharedKey = "mixed-key"

	var wg sync.WaitGroup
	results := make([]*domain.Transfer, numCallers)
	errs := make([]error, numCallers)

	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Half go to w2, half to w3 — different payload identities.
			to := "w2"
			if idx%2 == 1 {
				to = "w3"
			}
			tr, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
				IdempotencyKey: sharedKey,
				FromWalletID:   "w1",
				ToWalletID:     to,
				Amount:         100,
			})
			results[idx] = tr
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// Exactly ONE transfer exists.
	assert.Equal(t, 1, env.countTransfers())

	// Count outcomes. Whoever wins T1 sets the canonical payload; the other
	// half receive ErrIdempotencyKeyReuse.
	var successCount, conflictCount int
	for _, err := range errs {
		if err == nil {
			successCount++
		} else if errors.Is(err, domain.ErrIdempotencyKeyReuse) {
			conflictCount++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 25, successCount, "exactly the callers with the winning payload should succeed")
	assert.Equal(t, 25, conflictCount, "exactly the callers with the losing payload should see 409")

	env.assertInvariants()
}

// =============================================================================
// Race 3: Crossing transfers A→B and B→A — must not deadlock
//
// 100 transfers alternating direction between two wallets. With sorted FOR
// UPDATE locking (§8 Race 3), no deadlock can occur. Test passes if all
// transfers terminate within a reasonable time.
// =============================================================================

func TestConcurrency_CrossingTransfers_NoDeadlock(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("a", 100000)
	env.seedWallet("b", 100000)

	const numTransfers = 100

	var wg sync.WaitGroup
	results := make([]*domain.Transfer, numTransfers)
	errs := make([]error, numTransfers)

	for i := 0; i < numTransfers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			from, to := "a", "b"
			if idx%2 == 1 {
				from, to = "b", "a"
			}
			tr, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
				IdempotencyKey: uuid.New().String(),
				FromWalletID:   from,
				ToWalletID:     to,
				Amount:         10,
			})
			results[idx] = tr
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// No deadlocks, no errors.
	for i, err := range errs {
		require.NoError(t, err, "transfer %d errored (possible deadlock): %v", i, err)
	}

	// All transfers PROCESSED (both wallets had ample balance).
	for _, tr := range results {
		require.NotNil(t, tr)
		assert.Equal(t, domain.StatusProcessed, tr.Status)
	}

	// 50 a→b of 10 = -500 from a; 50 b→a of 10 = +500 to a. Net: zero.
	assert.Equal(t, int64(100000), env.walletBalance("a"))
	assert.Equal(t, int64(100000), env.walletBalance("b"))

	env.assertInvariants()
}

// =============================================================================
// Replay-runs-T2: simulate the case where T1 committed but T2 never ran,
// then a "retry" arrives. The retry handler should run T2 in-line.
//
// We don't have direct access to interrupt T2 mid-flight in a unit test, so
// we simulate it by:
//   1. Directly inserting a PENDING transfer (bypassing T2).
//   2. Calling Execute with the same idempotency key.
// The retry handler hits unique_violation in T1, falls into REPLAY, sees
// PENDING, and runs T2.
// =============================================================================

func TestConcurrency_RetryRunsT2_OnPending(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Directly insert a PENDING transfer to simulate "T1 committed, T2 never ran."
	const stalKey = "stale-pending-1"
	tid := uuid.New()
	_, err := env.pool.Exec(ctx, `
		INSERT INTO transfers (id, idempotency_key, payload_hash,
		                       from_wallet_id, to_wallet_id, amount, status)
		VALUES ($1, $2, $3, 'w1', 'w2', 100, 'PENDING')
	`, tid, stalKey, service.CanonicalPayloadHash("w1", "w2", 100))
	require.NoError(t, err)

	// Now a "retry" arrives with the same key. The retry hits unique_violation,
	// falls into REPLAY, finds PENDING, and runs T2 on the existing row.
	tr, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: stalKey,
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.NotNil(t, tr)
	assert.Equal(t, tid, tr.ID, "retry must operate on the existing PENDING row")
	assert.Equal(t, domain.StatusProcessed, tr.Status)
	assert.Equal(t, int64(400), env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.walletBalance("w2"))

	env.assertInvariants()
}

// =============================================================================
// Recovery worker can pick up stranded PENDING transfers
// =============================================================================

func TestConcurrency_RecoveryWorker_DrivesPendingToTerminal(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Directly insert a stale PENDING transfer (created_at well in the past).
	tid := uuid.New()
	_, err := env.pool.Exec(ctx, `
		INSERT INTO transfers (id, idempotency_key, payload_hash,
		                       from_wallet_id, to_wallet_id, amount, status, created_at)
		VALUES ($1, $2, $3, 'w1', 'w2', 100, 'PENDING', now() - interval '5 minutes')
	`, tid, "stranded-key", service.CanonicalPayloadHash("w1", "w2", 100))
	require.NoError(t, err)

	// Run the recovery worker with a 60s threshold; should pick up the stranded transfer.
	n, err := env.svc.RunRecoveryOnce(ctx, 60, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "recovery worker should have processed exactly 1 transfer")

	// Verify money moved.
	assert.Equal(t, int64(400), env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.walletBalance("w2"))

	env.assertInvariants()
}
