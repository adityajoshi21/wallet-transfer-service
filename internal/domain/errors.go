package domain

import "errors"

// Validation / input errors — these map to 400 at the HTTP layer.
var (
	ErrEmptyIdempotencyKey   = errors.New("idempotency key must be non-empty")
	ErrIdempotencyKeyTooLong = errors.New("idempotency key exceeds 128 chars")
	ErrAmountNotPositive     = errors.New("amount must be a positive integer")
	ErrSameWallet            = errors.New("fromWalletId must differ from toWalletId")
	ErrEmptyWalletID         = errors.New("wallet id must be non-empty")
)

// Reference / resource errors.
var (
	ErrWalletNotFound   = errors.New("wallet not found")
	ErrTransferNotFound = errors.New("transfer not found")
)

// Idempotency errors.
var (
	// Same key, different payload — client bug, fail loudly.
	ErrIdempotencyKeyReuse = errors.New("idempotency key reused with a different payload")
)

// State-machine errors — these indicate a programming error or race.
var (
	ErrIllegalStateTransition = errors.New("illegal transfer state transition")
	ErrStateAnomaly           = errors.New("transfer state changed unexpectedly during execution")
)

// Domain failure reasons. These are NOT errors — they are valid terminal
// outcomes that produce a FAILED transfer record (returned as 201).
const (
	FailureReasonInsufficientFunds = "INSUFFICIENT_FUNDS"
)
