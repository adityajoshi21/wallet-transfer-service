package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

// TransferHandler is the HTTP handler for /transfers.

// As per the brief's architecture rules: this layer does request validation,
// transport mapping, and invokes the service. No business logic. No SQL txns.
type TransferHandler struct {
	svc *service.TransferService
}

func NewTransferHandler(svc *service.TransferService) *TransferHandler {
	return &TransferHandler{svc: svc}
}

type createTransferRequest struct {
	FromWalletID string `json:"fromWalletId"`
	ToWalletID   string `json:"toWalletId"`
	Amount       int64  `json:"amount"`
}

type transferResponse struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	FromWalletID  string  `json:"fromWalletId"`
	ToWalletID    string  `json:"toWalletId"`
	Amount        int64   `json:"amount"`
	FailureReason *string `json:"failureReason"`
	CreatedAt     string  `json:"createdAt"`
	ProcessedAt   *string `json:"processedAt"`
}

type errorResponse struct {
	Error      string  `json:"error"`
	Detail     string  `json:"detail,omitempty"`
	TransferID *string `json:"transferId,omitempty"`
}

// IdempotencyKeyHeader is the canonical HTTP header for the idempotency key.
// We follow the Stripe convention: the key lives in the transport layer, not
// the payload. This keeps the payload identity (from/to/amount) cleanly
// separable from the request identity (key), which is the conceptual basis
// for `payload_hash` (see §9 of DESIGN.md).
const IdempotencyKeyHeader = "Idempotency-Key"

// =============================================================================
// POST /transfers
// =============================================================================

func (h *TransferHandler) CreateTransfer(w http.ResponseWriter, r *http.Request) {
	// Idempotency key from header. Missing → 400 with a specific error code
	// so the client can distinguish "you forgot the header" from "your payload
	// is malformed."
	idempotencyKey := r.Header.Get(IdempotencyKeyHeader)
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY",
			"Idempotency-Key header is required")
		return
	}

	// Parse JSON body.
	var req createTransferRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid JSON body: "+err.Error())
		return
	}

	// Hand off to service. All structural validation (non-positive amount,
	// same wallet, empty fields, key length) lives in the domain entity
	// (NewTransfer), so we don't duplicate it here.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	transfer, err := h.svc.ExecuteNewTransfer(ctx, service.ExecuteCommand{
		IdempotencyKey: idempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
	})

	if err != nil {
		mapServiceError(w, err)
		return
	}

	writeTransfer(w, http.StatusCreated, transfer)
}

// =============================================================================
// Error mapping (the only place HTTP status codes are assigned)
// =============================================================================

func mapServiceError(w http.ResponseWriter, err error) {
	switch {
	// 400 — domain validation errors (pre-T1, no transfer created).
	case errors.Is(err, domain.ErrEmptyIdempotencyKey),
		errors.Is(err, domain.ErrIdempotencyKeyTooLong),
		errors.Is(err, domain.ErrAmountNotPositive),
		errors.Is(err, domain.ErrSameWallet),
		errors.Is(err, domain.ErrEmptyWalletID):
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())

	// 404 — referenced wallet doesn't exist (FK violation at T1).
	case errors.Is(err, domain.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", err.Error())

	// 409 — idempotency key reused with a different payload.
	case errors.Is(err, domain.ErrIdempotencyKeyReuse):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSE", err.Error())

	// 500 — state-machine anomaly (would indicate a serious bug).
	case errors.Is(err, domain.ErrStateAnomaly):
		writeError(w, http.StatusInternalServerError, "STATE_ANOMALY", err.Error())

	// 500 — everything else.
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// =============================================================================
// Response writers
// =============================================================================

func writeTransfer(w http.ResponseWriter, status int, t *domain.Transfer) {
	writeJSON(w, status, transferToResponse(t))
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
