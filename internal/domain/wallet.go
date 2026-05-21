package domain

import "time"

// Invariant I4 (balance >= 0) is enforced in two places:
//  1. The Debit method below rejects withdrawals that would overdraw.
//  2. The DB CHECK constraint on wallets.balance as a backstop.

// Balance is stored in minor units (cents) as int64 to avoid floating-point drifts.
type Wallet struct {
	ID        string
	Balance   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Debit reduces the wallet's balance. Returns an error if the amount
// would cause the balance to go negative (I4) or is non-positive.
func (w *Wallet) Debit(amount int64) error {
	if amount <= 0 {
		return ErrAmountNotPositive
	}
	if w.Balance < amount {
		// Not an internal error; the service layer turns this into a
		// FAILED transfer with reason INSUFFICIENT_FUNDS.
		return ErrInsufficientFunds
	}
	w.Balance -= amount
	return nil
}

// Credit increases the wallet's balance. Rejects non-positive amounts.
func (w *Wallet) Credit(amount int64) error {
	if amount <= 0 {
		return ErrAmountNotPositive
	}
	w.Balance += amount
	return nil
}

// ErrInsufficientFunds is the domain-layer status that a debit was rejected
// because the wallet didn't have enough. The service layer catches this and
// translates it into a FAILED transfer with FailureReasonInsufficientFunds —
// it is NOT propagated as an HTTP error.
// This is a domain error, not an internal error, so its handling is not in the service layer.
var ErrInsufficientFunds = &insufficientFundsError{}

type insufficientFundsError struct{}

func (e *insufficientFundsError) Error() string { return "insufficient funds" }
