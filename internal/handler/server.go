package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter wires the transfer endpoint and middleware into a chi router.
func NewRouter(transferHandler *TransferHandler, walletHandler *WalletHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Post("/transfers", transferHandler.CreateTransfer)            //Endpoint for creating a transfer
	r.Get("/wallets/{id}", walletHandler.GetWalletDetails)          //Endpoint for getting wallet details
	r.Get("/wallets/{id}/transfers", walletHandler.ListTransfers)   //Endpoint for listing transfers history related to a wallet
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) { // Simple health check endpoint
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	return r
}
