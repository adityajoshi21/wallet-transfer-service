package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

// =============================================================================
// Happy path
// =============================================================================

func TestIntegration_HappyPath(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	tr, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "key-happy-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.NotNil(t, tr)

	assert.Equal(t, domain.StatusProcessed, tr.Status)
	assert.Nil(t, tr.FailureReason)
	assert.NotNil(t, tr.ProcessedAt)

	assert.Equal(t, int64(400), env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.walletBalance("w2"))
	assert.Equal(t, 1, env.countTransfers())
	assert.Equal(t, 2, env.countLedgerEntries())

	env.assertInvariants()
}

// =============================================================================
// FAILED: insufficient funds is a terminal outcome, not an error
// =============================================================================

func TestIntegration_InsufficientFunds(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 50)
	env.seedWallet("w2", 0)

	tr, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "key-fail-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)
	require.NotNil(t, tr)

	assert.Equal(t, domain.StatusFailed, tr.Status)
	require.NotNil(t, tr.FailureReason)
	assert.Equal(t, domain.FailureReasonInsufficientFunds, *tr.FailureReason)

	// Balances unchanged.
	assert.Equal(t, int64(50), env.walletBalance("w1"))
	assert.Equal(t, int64(0), env.walletBalance("w2"))
	// Transfer row exists, but no ledger entries (I6).
	assert.Equal(t, 1, env.countTransfers())
	assert.Equal(t, 0, env.countLedgerEntries())

	env.assertInvariants()
}

// =============================================================================
// Idempotency: same key + same body → identical result, no duplicate effect
// =============================================================================

func TestIntegration_IdempotencyReplay_SamePayload(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	cmd := service.ExecuteCommand{
		IdempotencyKey: "key-replay-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	}

	// First call: creates the transfer.
	first, err := env.svc.ExecuteNewTransfer(context.Background(), cmd)
	require.NoError(t, err)

	// Second call with the same key + same body: must return the same transfer.
	second, err := env.svc.ExecuteNewTransfer(context.Background(), cmd)
	require.NoError(t, err)

	// Identical transfer ID, identical status, identical balances.
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.Status, second.Status)
	assert.Equal(t, int64(400), env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.walletBalance("w2"))

	// Only ONE transfer + ONE ledger pair exist — duplicate request did NOT
	// produce a duplicate effect.
	assert.Equal(t, 1, env.countTransfers())
	assert.Equal(t, 2, env.countLedgerEntries())

	env.assertInvariants()
}

// =============================================================================
// Idempotency: same key + DIFFERENT body → 409 client bug
// =============================================================================

func TestIntegration_IdempotencyReplay_DifferentPayload(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)
	env.seedWallet("w3", 0)

	// First call.
	_, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "key-collision-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)

	// Second call: same key, but transferring to w3 instead of w2.
	_, err = env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "key-collision-1",
		FromWalletID:   "w1",
		ToWalletID:     "w3",
		Amount:         100,
	})
	assert.True(t, errors.Is(err, domain.ErrIdempotencyKeyReuse), "expected ErrIdempotencyKeyReuse, got %v", err)

	// Original transfer is untouched. No additional side effects.
	assert.Equal(t, int64(400), env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.walletBalance("w2"))
	assert.Equal(t, int64(0), env.walletBalance("w3"))
	assert.Equal(t, 1, env.countTransfers())
	assert.Equal(t, 2, env.countLedgerEntries())

	env.assertInvariants()
}

// =============================================================================
// Replay of a FAILED transfer is also idempotent
// =============================================================================

func TestIntegration_IdempotencyReplay_FailedTransfer(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 50) // not enough
	env.seedWallet("w2", 0)

	cmd := service.ExecuteCommand{
		IdempotencyKey: "key-replay-failed",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	}

	first, err := env.svc.ExecuteNewTransfer(context.Background(), cmd)
	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, first.Status)

	second, err := env.svc.ExecuteNewTransfer(context.Background(), cmd)
	require.NoError(t, err)

	// Same FAILED transfer is returned; nothing changed.
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, domain.StatusFailed, second.Status)
	assert.Equal(t, 1, env.countTransfers())
	assert.Equal(t, 0, env.countLedgerEntries())

	env.assertInvariants()
}

// =============================================================================
// Validation errors do not create transfer rows (and are not cached)
// =============================================================================

func TestIntegration_ValidationErrors(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)

	// Same wallet
	_, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "k-val-1",
		FromWalletID:   "w1",
		ToWalletID:     "w1",
		Amount:         100,
	})
	assert.True(t, errors.Is(err, domain.ErrSameWallet))

	// Non-positive amount
	_, err = env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "k-val-2",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         0,
	})
	assert.True(t, errors.Is(err, domain.ErrAmountNotPositive))

	// Empty key
	_, err = env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	assert.True(t, errors.Is(err, domain.ErrEmptyIdempotencyKey))

	// No transfer row exists for any of these.
	assert.Equal(t, 0, env.countTransfers())
}

// =============================================================================
// Wallet not found → 404, no transfer row
// =============================================================================

func TestIntegration_WalletNotFound(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	// w2 deliberately not created

	_, err := env.svc.ExecuteNewTransfer(context.Background(), service.ExecuteCommand{
		IdempotencyKey: "k-notfound-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	assert.True(t, errors.Is(err, domain.ErrWalletNotFound))
	assert.Equal(t, 0, env.countTransfers(), "no transfer row should exist for an unresolvable wallet")
}
