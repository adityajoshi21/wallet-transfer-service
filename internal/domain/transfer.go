package domain

import (
	"time"

	"github.com/google/uuid"
)

// TransferStatus is the status of a transfer in its lifecycle.
type TransferStatus string

const (
	StatusPending   TransferStatus = "PENDING"
	StatusProcessed TransferStatus = "PROCESSED"
	StatusFailed    TransferStatus = "FAILED"
)

// IsTerminal returns true if the status is a terminal state (PROCESSED or FAILED).
// Terminal states are immutable (I7).
func (s TransferStatus) IsTerminal() bool {
	return s == StatusProcessed || s == StatusFailed
}

// Transfer is the domain entity for a money transfer.

// The status machine is owned by this entity: only the MarkProcessed and
// MarkFailed methods can transition status, and they validate preconditions.
// This is what makes I7 (status monotonicity) impossible to violate via the
// domain layer; the SQL conditional UPDATE WHERE status='PENDING' is the
// backstop for any code path that bypasses the entity.
type Transfer struct {
	ID             uuid.UUID
	IdempotencyKey string
	PayloadHash    string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
	Status         TransferStatus
	FailureReason  *string
	CreatedAt      time.Time
	ProcessedAt    *time.Time
}

// NewTransfer constructs a new Transfer in PENDING state with validated inputs.
func NewTransfer(
	id uuid.UUID,
	idempotencyKey string,
	payloadHash string,
	fromWalletID string,
	toWalletID string,
	amount int64,
) (*Transfer, error) {
	if idempotencyKey == "" {
		return nil, ErrEmptyIdempotencyKey
	}
	if len(idempotencyKey) > 128 {
		return nil, ErrIdempotencyKeyTooLong
	}
	if amount <= 0 {
		return nil, ErrAmountNotPositive
	}
	if fromWalletID == "" || toWalletID == "" {
		return nil, ErrEmptyWalletID
	}
	if fromWalletID == toWalletID {
		return nil, ErrSameWallet
	}
	return &Transfer{
		ID:             id,
		IdempotencyKey: idempotencyKey,
		PayloadHash:    payloadHash,
		FromWalletID:   fromWalletID,
		ToWalletID:     toWalletID,
		Amount:         amount,
		Status:         StatusPending,
	}, nil
}

// MarkProcessed transitions a PENDING transfer to PROCESSED.
// Rejects any other source state.
func (t *Transfer) MarkProcessed(now time.Time) error {
	if t.Status != StatusPending {
		return ErrIllegalStateTransition
	}
	t.Status = StatusProcessed
	t.ProcessedAt = &now
	return nil
}

// MarkFailed transitions a PENDING transfer to FAILED with a reason.
// Rejects any other source state.
func (t *Transfer) MarkFailed(reason string, now time.Time) error {
	if t.Status != StatusPending {
		return ErrIllegalStateTransition
	}
	t.Status = StatusFailed
	t.FailureReason = &reason
	t.ProcessedAt = &now
	return nil
}

// IsTerminal returns true if the transfer is in a terminal state.
func (t *Transfer) IsTerminal() bool {
	return t.Status.IsTerminal()
}
