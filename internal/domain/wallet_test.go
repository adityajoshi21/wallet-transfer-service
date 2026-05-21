package domain_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wallet-transfer-service/internal/domain"
)

// Tests for a user's wallet — the balance and debit/credit operations.
// The critical invariant is that Debit must never succeed if the wallet has insufficient funds, and balance must be unchanged on failure.
func TestWallet_Debit_Success(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 500}
	err := w.Debit(100)
	assert.NoError(t, err)
	assert.Equal(t, int64(400), w.Balance)
}

// Test that a debit that exactly matches the balance is allowed, leaving a zero balance.
func TestWallet_Debit_ExactBalance(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 100}
	err := w.Debit(100)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), w.Balance)
}

// Debit that exceeds the balance is rejected with ErrInsufficientFunds, and balance is unchanged.
func TestWallet_Debit_InsufficientFunds(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 50}
	err := w.Debit(100)
	assert.True(t, errors.Is(err, domain.ErrInsufficientFunds))
	// I4: balance is unchanged on failure
	assert.Equal(t, int64(50), w.Balance)
}

// Debit with zero balance is rejected and balance is unchanged.
func TestWallet_Debit_ZeroBalance(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 0}
	err := w.Debit(1)
	assert.True(t, errors.Is(err, domain.ErrInsufficientFunds))
	assert.Equal(t, int64(0), w.Balance)
}

// Transfer amount must be positive <Debit> Zero or negative amounts are rejected.
func TestWallet_Debit_NonPositiveAmount(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 100}
	for _, a := range []int64{0, -1, -100} {
		err := w.Debit(a)
		assert.Equal(t, domain.ErrAmountNotPositive, err)
		assert.Equal(t, int64(100), w.Balance, "balance must be unchanged on rejected debit")
	}
}

// Successful credit increases the balance
func TestWallet_Credit_Success(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 0}
	err := w.Credit(250)
	assert.NoError(t, err)
	assert.Equal(t, int64(250), w.Balance)
}

// Transfer amount must be positive <Credit> Zero or negative amounts are rejected.
func TestWallet_Credit_NonPositiveAmount(t *testing.T) {
	w := &domain.Wallet{ID: "w1", Balance: 100}
	for _, a := range []int64{0, -1, -100} {
		err := w.Credit(a)
		assert.Equal(t, domain.ErrAmountNotPositive, err)
		assert.Equal(t, int64(100), w.Balance)
	}
}
