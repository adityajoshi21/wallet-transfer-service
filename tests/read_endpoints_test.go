package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

// =============================================================================
// GET wallet — returns current balance after transfers commit
// =============================================================================

func TestIntegration_GetWallet_ReflectsCurrentBalance(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 500)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Initial state.
	w, err := env.svc.GetWallet(ctx, "w1")
	require.NoError(t, err)
	assert.Equal(t, "w1", w.ID)
	assert.Equal(t, int64(500), w.Balance)
	assert.False(t, w.CreatedAt.IsZero())
	assert.False(t, w.UpdatedAt.IsZero())

	// After a transfer.
	_, err = env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "k-getwallet-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)

	w, err = env.svc.GetWallet(ctx, "w1")
	require.NoError(t, err)
	assert.Equal(t, int64(400), w.Balance)

	w, err = env.svc.GetWallet(ctx, "w2")
	require.NoError(t, err)
	assert.Equal(t, int64(100), w.Balance)
}

func TestIntegration_GetWallet_NotFound(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.svc.GetWallet(context.Background(), "nonexistent")
	assert.True(t, errors.Is(err, domain.ErrWalletNotFound))
}

// =============================================================================
// List wallet transfers — history includes PROCESSED, FAILED, and PENDING;
// both sides (source and destination) are returned
// =============================================================================

func TestIntegration_ListWalletTransfers_BothSidesIncluded(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 1000)
	env.seedWallet("w2", 1000)
	env.seedWallet("w3", 0)

	ctx := context.Background()

	// w1 -> w2 (PROCESSED)
	_, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "hist-1",
		FromWalletID:   "w1",
		ToWalletID:     "w2",
		Amount:         100,
	})
	require.NoError(t, err)

	// w2 -> w1 (PROCESSED) — w1 is destination here
	_, err = env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "hist-2",
		FromWalletID:   "w2",
		ToWalletID:     "w1",
		Amount:         50,
	})
	require.NoError(t, err)

	// w1 -> w3 of 9999 (FAILED — insufficient)
	_, err = env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: "hist-3",
		FromWalletID:   "w1",
		ToWalletID:     "w3",
		Amount:         9999,
	})
	require.NoError(t, err)

	// History for w1 should include all 3 transfers (2 as source, 1 as dest).
	got, err := env.svc.ListWalletTransfers(ctx, service.ListWalletTransfersInput{
		WalletID: "w1",
		Limit:    50,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, len(got), "w1 should appear in all 3 transfers")

	// Verify a mix of statuses present.
	statuses := map[domain.TransferStatus]int{}
	for _, tr := range got {
		statuses[tr.Status]++
	}
	assert.Equal(t, 2, statuses[domain.StatusProcessed])
	assert.Equal(t, 1, statuses[domain.StatusFailed])

	// History for w3: only the FAILED one (w3 was destination of a failed transfer).
	// Important: FAILED transfers DO appear in history even though no money moved.
	// The transfer row was committed; the wallet was referenced by it.
	got, err = env.svc.ListWalletTransfers(ctx, service.ListWalletTransfersInput{
		WalletID: "w3",
		Limit:    50,
	})
	require.NoError(t, err)
	require.Equal(t, 1, len(got))
	assert.Equal(t, domain.StatusFailed, got[0].Status)
}

func TestIntegration_ListWalletTransfers_OrderedNewestFirst(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 10000)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Create 5 transfers with small gaps to guarantee distinct created_at.
	for i := 0; i < 5; i++ {
		_, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
			IdempotencyKey: "order-" + uuid.New().String(),
			FromWalletID:   "w1",
			ToWalletID:     "w2",
			Amount:         10,
		})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}

	got, err := env.svc.ListWalletTransfers(ctx, service.ListWalletTransfersInput{
		WalletID: "w1",
		Limit:    50,
	})
	require.NoError(t, err)
	require.Equal(t, 5, len(got))

	// Newest first.
	for i := 1; i < len(got); i++ {
		assert.True(t,
			got[i-1].CreatedAt.After(got[i].CreatedAt) ||
				got[i-1].CreatedAt.Equal(got[i].CreatedAt),
			"transfer at idx %d should be older than idx %d", i, i-1,
		)
	}
}

func TestIntegration_ListWalletTransfers_Pagination(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 10000)
	env.seedWallet("w2", 0)

	ctx := context.Background()

	// Create 5 transfers.
	for i := 0; i < 5; i++ {
		_, err := env.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
			IdempotencyKey: "page-" + uuid.New().String(),
			FromWalletID:   "w1",
			ToWalletID:     "w2",
			Amount:         10,
		})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}

	// First page: limit 2.
	page1, err := env.svc.ListWalletTransfers(ctx, service.ListWalletTransfersInput{
		WalletID: "w1",
		Limit:    2,
	})
	require.NoError(t, err)
	require.Equal(t, 2, len(page1))

	// Second page using the oldest of page 1 as the cursor.
	page2, err := env.svc.ListWalletTransfers(ctx, service.ListWalletTransfersInput{
		WalletID: "w1",
		Before:   page1[1].CreatedAt,
		Limit:    2,
	})
	require.NoError(t, err)
	require.Equal(t, 2, len(page2))

	// No overlap between pages.
	for _, p1 := range page1 {
		for _, p2 := range page2 {
			assert.NotEqual(t, p1.ID, p2.ID, "pages must not overlap")
		}
	}
}

func TestIntegration_ListWalletTransfers_EmptyHistoryDistinctFrom404(t *testing.T) {
	env := newTestEnv(t)
	env.seedWallet("w1", 0) // exists but no transfers

	got, err := env.svc.ListWalletTransfers(context.Background(), service.ListWalletTransfersInput{
		WalletID: "w1",
		Limit:    50,
	})
	require.NoError(t, err, "existing wallet with no transfers should not be a 404")
	assert.Empty(t, got)
}

func TestIntegration_ListWalletTransfers_WalletNotFound(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.svc.ListWalletTransfers(context.Background(), service.ListWalletTransfersInput{
		WalletID: "nonexistent",
		Limit:    50,
	})
	assert.True(t, errors.Is(err, domain.ErrWalletNotFound))
}
