package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/logger"
	"github.com/wallet-transfer-service/internal/repository"
)

// =============================================================================
// Service contract
// =============================================================================

// ExecuteCommand is the input to TransferService.Execute. The handler is
// responsible for converting incoming HTTP requests into this shape before
// invoking the service.
type ExecuteCommand struct {
	IdempotencyKey string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
}

// TransferService orchestrates the wallet-transfer flow.
//
// It is the OWNER of transaction boundaries (§14): it begins, commits, and
// rolls back transactions. Repositories never start a transaction on their
// own; they only accept one as a parameter.
type TransferService struct {
	pool      *pgxpool.Pool
	transfers *repository.TransferRepository
	wallets   *repository.WalletRepository
	ledger    *repository.LedgerRepository
	now       func() time.Time // injectable for tests
}

// NewTransferService constructs a TransferService with the given dependencies.
func NewTransferService(
	pool *pgxpool.Pool,
	transfers *repository.TransferRepository,
	wallets *repository.WalletRepository,
	ledger *repository.LedgerRepository,
) *TransferService {
	return &TransferService{
		pool:      pool,
		transfers: transfers,
		wallets:   wallets,
		ledger:    ledger,
		now:       time.Now,
	}
}

// =============================================================================
// Public entry point
// =============================================================================

// ExecuteNewTransfer runs the full transfer flow: T1 (claim idempotency key)
// → T2(atomic money movement), with REPLAY as the path for duplicate requests.

func (s *TransferService) ExecuteNewTransfer(ctx context.Context, cmd ExecuteCommand) (*domain.Transfer, error) {
	log := logger.Component("service.transfer").With(
		"from_wallet_id", cmd.FromWalletID,
		"to_wallet_id", cmd.ToWalletID,
		"amount", cmd.Amount,
	)

	payloadHash := CanonicalPayloadHash(cmd.FromWalletID, cmd.ToWalletID, cmd.Amount)

	// Build the domain entity. This runs structural validation (non-positive
	// amount, same wallet, empty fields) before any DB work — those errors
	// surface as 400 at the handler with no transfer row written.
	transfer, err := domain.NewTransfer(
		uuid.New(),
		cmd.IdempotencyKey,
		payloadHash,
		cmd.FromWalletID,
		cmd.ToWalletID,
		cmd.Amount,
	)
	if err != nil {
		log.Debug("validation rejected at domain layer", "error", err.Error())
		return nil, err
	}
	log = log.With("transfer_id: ", transfer.ID.String(), "payload_hash: ", payloadHash[:12])

	// -----------------------------------------------------------------
	// T1 — Reserve the idempotency key
	// -----------------------------------------------------------------
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Error("T1 begin failed", "error", err.Error())
		return nil, fmt.Errorf("begin T1: %w", err)
	}
	rollback := true
	// We defer a rollback in every code path. If we commit successfully,
	// we set `rollback = false` to turn the deferred rollback into a no-op.
	// we never accidentally rollback after a successful commit on T1.
	defer func() {
		if rollback {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := s.transfers.Insert(ctx, tx, transfer); err != nil {
		if repository.IsUniqueViolation(err) {
			// Idempotency key already taken
			// Rollback this T1 attempt; as REPLAY runs in its own transaction.
			_ = tx.Rollback(ctx)
			rollback = false
			log.Debug("T1 unique violation — routing to REPLAY")
			return s.replay(ctx, cmd.IdempotencyKey, payloadHash)
		}
		if repository.IsForeignKeyViolation(err) {
			// One of the wallets doesn't exist → 404, no transfer cached.
			log.Info("T1 FK violation — wallet not found")
			return nil, domain.ErrWalletNotFound
		}
		log.Error("T1 insert failed", "error", err.Error())
		return nil, fmt.Errorf("T1 insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		log.Error("T1 commit failed", "error", err.Error())
		return nil, fmt.Errorf("T1 commit: %w", err)
	}
	rollback = false
	log.Debug("T1 committed — key reserved, transfer PENDING")

	return s.executeT2(ctx, transfer.ID)
}

// =============================================================================
// T2 — Money movement
// =============================================================================

// executeT2 runs the money-movement transaction. Called by Execute after a
// successful T1, or by the recovery worker for stale PENDING transfers.
func (s *TransferService) executeT2(ctx context.Context, transferID uuid.UUID) (*domain.Transfer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin T2: %w", err)
	}
	defer tx.Rollback(ctx) // safe call after commit

	// Lock the transfer row.
	transfer, err := s.transfers.GetByIDForUpdate(ctx, tx, transferID)
	if err != nil {
		return nil, fmt.Errorf("T2 lock transfer: %w", err)
	}

	result, err := s.executeT2Locked(ctx, tx, transfer)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("T2 commit: %w", err)
	}
	return result, nil
}

// executeT2Locked runs T2's body given an ALREADY-LOCKED transfer row.
//
// This is the unification point for two entry paths (§7):
//  1. Happy path from Execute() — transfer was just inserted in T1, now we
//     lock it and run T2.
//  2. REPLAY found PENDING — the SELECT FOR UPDATE in REPLAY already locked
//     the row, and the same transaction continues straight into T2. (Recovery workers)

// In both cases this function operates inside the caller's transaction
// The caller is responsible for COMMIT.
func (s *TransferService) executeT2Locked(ctx context.Context, tx pgx.Tx, transfer *domain.Transfer) (*domain.Transfer, error) {
	log := logger.Component("service.transfer").With(
		"transfer_id", transfer.ID.String(),
		"from_wallet_id", transfer.FromWalletID,
		"to_wallet_id", transfer.ToWalletID,
		"amount", transfer.Amount,
	)

	// Defensive check for terminal status: if the transfer is already marked PROCESSED or FAILED
	if transfer.Status != domain.StatusPending {
		// Someone else terminated it — return the terminal record as-is.
		log.Debug("T2 short-circuit — transfer already terminal", "status", string(transfer.Status))
		return transfer, nil
	}

	// Lock both wallets in deterministic (ascending ID) order of wallet IDs.
	// The repository sorts internally — caller doesn't need to think about it.
	wallets, err := s.wallets.LockForUpdate(ctx, tx, []string{
		transfer.FromWalletID,
		transfer.ToWalletID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrWalletNotFound) {
			reason := "WALLET_NOT_FOUND"
			log.Warn("T2 wallet vanished between T1 and T2 — marking FAILED")
			if markFailedErr := transfer.MarkFailed(reason, s.now()); markFailedErr != nil {
				return nil, markFailedErr
			}
			if _, updateToTerminalErr := s.transfers.UpdateTerminalConditional(ctx, tx, transfer); updateToTerminalErr != nil {
				return nil, updateToTerminalErr
			}
			return transfer, nil
		}
		log.Error("T2 wallet lock failed", "error", err.Error())
		return nil, fmt.Errorf("T2 lock wallets: %w", err)
	}

	var from, to *domain.Wallet
	for _, w := range wallets {
		switch w.ID {
		case transfer.FromWalletID:
			from = w
		case transfer.ToWalletID:
			to = w
		}
	}
	// Defensive check: the locked wallets should include both from and to.
	if from == nil || to == nil {
		log.Error("T2 invariant violation — locked wallets missing from/to (should be impossible)")
		return nil, fmt.Errorf("T2: locked wallets do not include from/to wallets")
	}

	// Apply the debit on the source.
	if debitErr := from.Debit(transfer.Amount); debitErr != nil {
		if errors.Is(debitErr, domain.ErrInsufficientFunds) {
			if markFailedErr := transfer.MarkFailed(domain.FailureReasonInsufficientFunds, s.now()); markFailedErr != nil {
				return nil, markFailedErr
			}
			rows, updateTerminalErr := s.transfers.UpdateTerminalConditional(ctx, tx, transfer)
			if updateTerminalErr != nil {
				log.Error("T2 FAILED-state update failed", "error", updateTerminalErr.Error())
				return nil, updateTerminalErr
			}
			if rows != 1 {
				// I7 anomaly: someone else terminated the row between our
				// SELECT FOR UPDATE and this UPDATE. This is a serious bug.
				log.Error("T2 state anomaly — FAILED transition affected 0 rows",
					"reason:", "another transaction terminated this transfer while we held FOR UPDATE",
				)
				return nil, domain.ErrStateAnomaly
			}
			//early return on insufficient funds
			log.Info("T2 committed FAILED",
				"failure_reason", domain.FailureReasonInsufficientFunds,
				"from_balance", from.Balance,
			)
			return transfer, nil
		}
		log.Error("T2 debit failed unexpectedly", "error", debitErr.Error())
		return nil, fmt.Errorf("T2 debit: %w", debitErr)
	}

	// Apply the credit on the destination.
	if creditErr := to.Credit(transfer.Amount); creditErr != nil {
		log.Error("T2 credit failed unexpectedly", "error", creditErr.Error())
		return nil, fmt.Errorf("T2 credit: %w", creditErr)
	}

	// Write the ledger pair. UNIQUE(transfer_id, direction) catches any
	// double-execution at the DB level (I1 + executor-level idempotency).
	debit := domain.NewDebitEntry(transfer.ID, from.ID, transfer.Amount, from.Balance)
	credit := domain.NewCreditEntry(transfer.ID, to.ID, transfer.Amount, to.Balance)
	if err := s.ledger.InsertPair(ctx, tx, debit, credit); err != nil {
		log.Error("T2 ledger insert failed", "error", err.Error())
		return nil, fmt.Errorf("T2 ledger insert: %w", err)
	}

	// Update both balances. These are the same transaction as the ledger
	// insert, so I3 (balance ↔ ledger consistency) holds at every committed
	// snapshot.
	if err := s.wallets.UpdateBalance(ctx, tx, from); err != nil {
		log.Error("T2 from-balance update failed", "error", err.Error())
		return nil, fmt.Errorf("T2 update from-balance: %w", err)
	}
	if err := s.wallets.UpdateBalance(ctx, tx, to); err != nil {
		log.Error("T2 to-balance update failed", "error", err.Error())
		return nil, fmt.Errorf("T2 update to-balance: %w", err)
	}

	// Transition transfer to PROCESSED.
	if markProcessedErr := transfer.MarkProcessed(s.now()); markProcessedErr != nil {
		return nil, markProcessedErr
	}
	rows, err := s.transfers.UpdateTerminalConditional(ctx, tx, transfer)
	if err != nil {
		log.Error("T2 PROCESSED-state update failed", "error", err.Error())
		return nil, fmt.Errorf("T2 update transfer: %w", err)
	}
	if rows != 1 {
		log.Error("T2 state anomaly — PROCESSED transition affected 0 rows",
			"hint", "another transaction terminated this transfer while we held FOR UPDATE",
		)
		return nil, domain.ErrStateAnomaly
	}

	log.Info("T2 committed PROCESSED",
		"from_balance_after", from.Balance,
		"to_balance_after", to.Balance,
	)
	return transfer, nil
}

// =============================================================================
// REPLAY — pure-read path for duplicates requests (or in-line T2 if PENDING)
// =============================================================================

// replay handles a duplicate request: an identical idempotency_key was already
// used. The flow:
//
//  1. SELECT the existing transfer FOR UPDATE (locks the row).
//  2. If payload hashes don't match → 409 idempotency key reuse.
//  3. If status is terminal → return it (pure read, no side effects).
//  4. If status is PENDING → continue THE SAME TRANSACTION into T2's body,
//     running money movement on the existing transfer row. The lock from
//     step 1 carries through.
//
// This is what the design doc calls "REPLAY runs T2 inline" — eliminates the
// poll loop for retries that arrive while the original is mid-flight or
// the original crashed before T2.
func (s *TransferService) replay(ctx context.Context, key, incomingPayloadHash string) (*domain.Transfer, error) {
	log := logger.Component("service.transfer").With(
		"payload_hash", incomingPayloadHash[:12],
	)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Error("REPLAY begin failed", "error", err.Error())
		return nil, fmt.Errorf("begin replay: %w", err)
	}
	defer tx.Rollback(ctx)

	existing, err := s.transfers.GetByKeyForUpdate(ctx, tx, key)
	if err != nil {
		log.Error("REPLAY lookup failed", "error", err.Error())
		return nil, fmt.Errorf("replay lookup: %w", err)
	}

	log = log.With(
		"transfer_id", existing.ID.String(),
		"existing_status", string(existing.Status),
	)

	// Payload-identity check: same key + different payload = client bug.
	if existing.PayloadHash != incomingPayloadHash {
		// IMPORTANT: same idempotency key reused with a different
		// payload. Almost always a client bug — log loudly.
		log.Warn("REPLAY rejected — idempotency key reused with different payload",
			"existing_payload_hash", existing.PayloadHash[:12],
		)
		return nil, domain.ErrIdempotencyKeyReuse
	}

	// If terminal, return the cached outcome. This transaction does no writes;
	// the defer'd Rollback is a no-op for correctness.
	if existing.IsTerminal() {
		log.Info("REPLAY returned cached terminal outcome")
		return existing, nil
	}

	// PENDING: run T2 in this same transaction. We hold the FOR UPDATE lock
	// from the SELECT above, which carries straight through into the wallet
	// locks and ledger writes.
	log.Info("REPLAY found PENDING — running T2 inline")
	result, err := s.executeT2Locked(ctx, tx, existing)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		log.Error("REPLAY commit failed", "error", err.Error())
		return nil, fmt.Errorf("replay commit: %w", err)
	}
	return result, nil
}

// =============================================================================
// Recovery worker — the long-tail safety net (§13)
// =============================================================================

// RunRecoveryOnce picks up to `batchSize : 100(default)` PENDING transfers that
// are stranded for at least `olderThanSeconds` : 30s(default) and runs T2 on each.
//
// Safe to run from multiple processes concurrently because:
//   - ListStalePending uses FOR UPDATE SKIP LOCKED — workers can't double-pick.
//   - executeT2 re-checks status under FOR UPDATE — if it's already terminal
//     (e.g. the retry handler raced us), we return immediately.
//   - UNIQUE(transfer_id, direction) on the ledger is the absolute backstop.
func (s *TransferService) RunRecoveryOnce(ctx context.Context, olderThanSeconds, batchSize int) (int, error) {
	log := logger.Component("service.recovery").With(
		"threshold_seconds", olderThanSeconds,
		"batch_size", batchSize,
	)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Error("recovery begin failed", "error", err.Error())
		return 0, fmt.Errorf("recovery begin: %w", err)
	}
	candidates, err := s.transfers.ListStalePending(ctx, tx, olderThanSeconds, batchSize)
	if err != nil {
		_ = tx.Rollback(ctx)
		log.Error("recovery list failed", "error", err.Error())
		return 0, fmt.Errorf("recovery list: %w", err)
	}
	// We release the listing-tx lock (rollback is fine — we made no changes
	// other than acquiring SKIP LOCKED row locks, which release on rollback).
	// Then each candidate gets its own T2 transaction.
	_ = tx.Rollback(ctx)

	if len(candidates) == 0 {
		log.Debug("recovery sweep — no stranded transfers")
		return 0, nil
	}

	log.Info("recovery sweep starting", "candidates", len(candidates))

	processed := 0
	for _, t := range candidates {
		candLog := log.With("transfer_id", t.ID.String())
		if _, err := s.executeT2(ctx, t.ID); err != nil {
			// Log and continue; recovery is best-effort.
			candLog.Warn("recovery T2 errored — skipping", "error", err.Error())
			continue
		}
		candLog.Info("recovery T2 succeeded")
		processed++
	}

	log.Info("recovery sweep complete", "processed", processed, "skipped", len(candidates)-processed)
	return processed, nil
}

// =============================================================================
// Read endpoints — GET /wallets/{id}, GET /wallets/{id}/transfers
// =============================================================================

// GetWallet returns the full wallet record. Returns ErrWalletNotFound if no
// such wallet exists. This is a read-only operation; no locks acquired.
func (s *TransferService) GetWallet(ctx context.Context, id string) (*domain.Wallet, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin read: %w", err)
	}
	defer tx.Rollback(ctx)
	return s.wallets.GetByID(ctx, tx, id)
}

// ListWalletTransfersInput parameterizes the transfer-history query.
type ListWalletTransfersInput struct {
	WalletID string
	Before   time.Time // exclusive upper bound on created_at; zero for first page
	Limit    int       // capped at 100
}

// ListWalletTransfers returns recent transfers involving the given wallet
// (either as source or destination), most recent first.
func (s *TransferService) ListWalletTransfers(ctx context.Context, in ListWalletTransfersInput) ([]*domain.Transfer, error) {
	//defensive validation of input parameters, if service is called directly outside the handler
	if in.WalletID == "" {
		return nil, domain.ErrEmptyWalletID
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin read: %w", err)
	}
	defer tx.Rollback(ctx)

	// Confirm wallet exists first so we can return 404 distinguishably
	if _, err := s.wallets.GetByID(ctx, tx, in.WalletID); err != nil {
		return nil, err
	}

	return s.transfers.ListByWallet(ctx, tx, in.WalletID, in.Before, limit)
}
