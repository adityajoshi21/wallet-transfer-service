package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

// WalletHandler serves read-only wallet endpoints:

//	GET /wallets/{id}            -> current balance + metadata
//	GET /wallets/{id}/transfers  -> recent transfer history involving the wallet
//
// Both are pure reads with no idempotency requirements.
type WalletHandler struct {
	svc *service.TransferService
}

func NewWalletHandler(svc *service.TransferService) *WalletHandler {
	return &WalletHandler{svc: svc}
}

type walletResponse struct {
	ID        string `json:"id"`
	Balance   int64  `json:"balance"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type listTransfersResponse struct {
	WalletID  string             `json:"walletId"`
	Count     int                `json:"count"`
	Transfers []transferResponse `json:"transfers"`
	// `before` query param to fetch the next page. RFC3339 timestamp of the oldest transfer in this page.
	NextBefore *string `json:"nextBefore,omitempty"`
}

// =============================================================================
// GET /wallets/{id}
// =============================================================================

func (h *WalletHandler) GetWalletDetails(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "wallet id is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	wallet, err := h.svc.GetWallet(ctx, id)
	if err != nil {
		mapReadError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, walletResponse{
		ID:        wallet.ID,
		Balance:   wallet.Balance,
		CreatedAt: wallet.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: wallet.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// =============================================================================
// GET /wallets/{id}/transfers
// =============================================================================

func (h *WalletHandler) ListTransfers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "wallet id is required")
		return
	}

	// Parse optional query params.
	q := r.URL.Query()

	limit := 50 // default page size
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "VALIDATION", "limit must be a valid integer between 1 and 100")
			return
		}
		limit = n
	}

	var before time.Time
	if raw := q.Get("before"); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION", "before must be a valid RFC3339 timestamp")
			return
		}
		before = t
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	transfers, err := h.svc.ListWalletTransfers(ctx, service.ListWalletTransfersInput{
		WalletID: id,
		Before:   before,
		Limit:    limit,
	})
	if err != nil {
		mapReadError(w, err)
		return
	}

	// Build response. If we got exactly `limit` rows, there may be more — set
	// NextBefore to the oldest returned transfer's created_at so the client
	// can request from the next page onwards.
	out := listTransfersResponse{
		WalletID:  id,
		Count:     len(transfers),
		Transfers: make([]transferResponse, 0, len(transfers)),
	}
	for _, t := range transfers {
		out.Transfers = append(out.Transfers, transferToResponse(t))
	}
	if len(transfers) == limit && len(transfers) > 0 {
		oldest := transfers[len(transfers)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
		out.NextBefore = &oldest
	}

	writeJSON(w, http.StatusOK, out)
}

// =============================================================================
// Helpers
// =============================================================================

// mapReadError handles the smaller error vocabulary of read endpoints.
func mapReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrEmptyWalletID):
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// transferToResponse converts a domain.Transfer to the wire response.
// Kept in the wallet handler file because both handlers produce it;
// it's available to both as they share the package.
func transferToResponse(t *domain.Transfer) transferResponse {
	resp := transferResponse{
		ID:           t.ID.String(),
		Status:       string(t.Status),
		FromWalletID: t.FromWalletID,
		ToWalletID:   t.ToWalletID,
		Amount:       t.Amount,
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.FailureReason != nil {
		resp.FailureReason = t.FailureReason
	}
	if t.ProcessedAt != nil {
		s := t.ProcessedAt.UTC().Format(time.RFC3339Nano)
		resp.ProcessedAt = &s
	}
	return resp
}
