package domain

import (
	"time"

	"github.com/google/uuid"
)

// EntryDirection is whether a ledger entry is a debit or a credit.
type EntryDirection string

const (
	DirectionDebit  EntryDirection = "DEBIT"
	DirectionCredit EntryDirection = "CREDIT"
)

// LedgerEntry is one leg of a double-entry ledger record.
//
// A transfer produces exactly two entries: one DEBIT on the source wallet
// and one CREDIT on the destination wallet, both with the same amount.
// This is enforced structurally by UNIQUE(transfer_id, direction) and
// UNIQUE(transfer_id, wallet_id) — see §4.
//
// balance_after is the wallet's balance AFTER applying this entry. With
// this column, point-in-time balance reconstruction is O(1) per entry
// instead of O(history).
type LedgerEntry struct {
	ID           int64
	TransferID   uuid.UUID
	WalletID     string
	Direction    EntryDirection
	Amount       int64
	BalanceAfter int64
	CreatedAt    time.Time
}

// NewDebitEntry constructs a DEBIT entry for the source wallet.
func NewDebitEntry(transferID uuid.UUID, walletID string, amount, balanceAfter int64) LedgerEntry {
	return LedgerEntry{
		TransferID:   transferID,
		WalletID:     walletID,
		Direction:    DirectionDebit,
		Amount:       amount,
		BalanceAfter: balanceAfter,
	}
}

// NewCreditEntry constructs a CREDIT entry for the destination wallet.
func NewCreditEntry(transferID uuid.UUID, walletID string, amount, balanceAfter int64) LedgerEntry {
	return LedgerEntry{
		TransferID:   transferID,
		WalletID:     walletID,
		Direction:    DirectionCredit,
		Amount:       amount,
		BalanceAfter: balanceAfter,
	}
}
