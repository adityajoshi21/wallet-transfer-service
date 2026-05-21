package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/wallet-transfer-service/internal/domain"
)

// ---------- Test helpers ----------
// Creates a valid PENDING transfer with the given parameters. Fails the test if creation fails.
func newPendingTransfer(t *testing.T) *domain.Transfer {
	t.Helper()
	tr, err := domain.NewTransfer(uuid.New(), "key1", "hash1", "w1", "w2", 100)
	assert.NoError(t, err)
	assert.Equal(t, domain.StatusPending, tr.Status)
	return tr
}

// ---------- Construction ----------
// Tests for the NewTransfer constructor, which validates input and creates a new PENDING transfer.
func TestNewTransfer_Success(t *testing.T) {
	tr := newPendingTransfer(t)
	assert.Equal(t, "key1", tr.IdempotencyKey)
	assert.Equal(t, "hash1", tr.PayloadHash)
	assert.Equal(t, "w1", tr.FromWalletID)
	assert.Equal(t, "w2", tr.ToWalletID)
	assert.Equal(t, int64(100), tr.Amount)
	assert.Equal(t, domain.StatusPending, tr.Status)
	assert.Nil(t, tr.ProcessedAt)
	assert.Nil(t, tr.FailureReason)
}

// Tests for invalid input parameters to NewTransfer.
func TestNewTransfer_RejectsEmptyKey(t *testing.T) {
	_, err := domain.NewTransfer(uuid.New(), "", "h", "w1", "w2", 100)
	assert.Equal(t, domain.ErrEmptyIdempotencyKey, err)
}

// 128 chars is the max length for the idempotency key (see database schema).
// This test ensures that the constructor enforces this limit.
func TestNewTransfer_RejectsLongKey(t *testing.T) {
	longKey := make([]byte, 129)
	for i := range longKey {
		longKey[i] = 'a'
	}
	_, err := domain.NewTransfer(uuid.New(), string(longKey), "h", "w1", "w2", 100)
	assert.Equal(t, domain.ErrIdempotencyKeyTooLong, err)
}

// Transfer amount must be positive. Zero or negative amounts are rejected.
func TestNewTransfer_RejectsNonPositiveAmount(t *testing.T) {
	for _, a := range []int64{0, -1, -100} {
		_, err := domain.NewTransfer(uuid.New(), "k", "h", "w1", "w2", a)
		assert.Equal(t, domain.ErrAmountNotPositive, err)
	}
}

// The same wallet cannot be both the source and destination of a transfer.
func TestNewTransfer_RejectsSameWallet(t *testing.T) {
	_, err := domain.NewTransfer(uuid.New(), "k", "h", "w1", "w1", 100)
	assert.Equal(t, domain.ErrSameWallet, err)
}

// Wallet IDs cannot be empty.
func TestNewTransfer_RejectsEmptyWalletIDs(t *testing.T) {
	_, err := domain.NewTransfer(uuid.New(), "k", "h", "", "w2", 100)
	assert.Equal(t, domain.ErrEmptyWalletID, err)
	_, err = domain.NewTransfer(uuid.New(), "k", "h", "w1", "", 100)
	assert.Equal(t, domain.ErrEmptyWalletID, err)
}

// ---------- State transitions: PENDING -> PROCESSED ----------
// Tests for the MarkProcessed method, which transitions a transfer from PENDING to PROCESSED.
func TestTransfer_MarkProcessed_FromPending(t *testing.T) {
	tr := newPendingTransfer(t)
	now := time.Now()
	err := tr.MarkProcessed(now)
	assert.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, tr.Status)
	assert.NotNil(t, tr.ProcessedAt)
	assert.Equal(t, now, *tr.ProcessedAt)
	assert.True(t, tr.IsTerminal())
}

// Once a transfer is PROCESSED, it cannot be marked PROCESSED again — test for illegal state transition.
func TestTransfer_MarkProcessed_FromProcessed_Rejected(t *testing.T) {
	tr := newPendingTransfer(t)
	_ = tr.MarkProcessed(time.Now())

	err := tr.MarkProcessed(time.Now())
	assert.Equal(t, domain.ErrIllegalStateTransition, err)
}

func TestTransfer_MarkProcessed_FromFailed_Rejected(t *testing.T) {
	tr := newPendingTransfer(t)
	_ = tr.MarkFailed("X", time.Now())

	err := tr.MarkProcessed(time.Now())
	assert.Equal(t, domain.ErrIllegalStateTransition, err)
}

// ---------- State transitions: PENDING -> FAILED ----------

func TestTransfer_MarkFailed_FromPending(t *testing.T) {
	tr := newPendingTransfer(t)
	now := time.Now()
	err := tr.MarkFailed(domain.FailureReasonInsufficientFunds, now)
	assert.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, tr.Status)
	assert.NotNil(t, tr.FailureReason)
	assert.Equal(t, domain.FailureReasonInsufficientFunds, *tr.FailureReason)
	assert.NotNil(t, tr.ProcessedAt)
	assert.True(t, tr.IsTerminal())
}

func TestTransfer_MarkFailed_FromProcessed_Rejected(t *testing.T) {
	tr := newPendingTransfer(t)
	_ = tr.MarkProcessed(time.Now())

	err := tr.MarkFailed("X", time.Now())
	assert.Equal(t, domain.ErrIllegalStateTransition, err)
}

func TestTransfer_MarkFailed_FromFailed_Rejected(t *testing.T) {
	tr := newPendingTransfer(t)
	_ = tr.MarkFailed("X", time.Now())

	err := tr.MarkFailed("Y", time.Now())
	assert.Equal(t, domain.ErrIllegalStateTransition, err)
}

// ---------- IsTerminal ----------

func TestTransferStatus_IsTerminal(t *testing.T) {
	assert.False(t, domain.StatusPending.IsTerminal())
	assert.True(t, domain.StatusProcessed.IsTerminal())
	assert.True(t, domain.StatusFailed.IsTerminal())
}
