package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wallet-transfer-service/internal/handler"
	"github.com/wallet-transfer-service/internal/logger"
	"github.com/wallet-transfer-service/internal/repository"
	"github.com/wallet-transfer-service/internal/service"
)

func main() {
	// Initialize the centralised logger FIRST so every subsequent log line is structured
	// Config via WALLET_LOG_LEVEL and WALLET_LOG_FORMAT env vars.
	logger.Init()
	log := logger.Component("main")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://wallet:wallet@localhost:5432/wallet_transfer?sslmode=disable"
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Wire dependencies bottom-up.
	pool, err := repository.NewPool(ctx, dsn)
	if err != nil {
		log.Error("db connect failed", "error", err.Error())
		os.Exit(1)
	}
	defer pool.Close()
	log.Info("db pool ready", "dsn_host", maskDSN(dsn))

	transferRepo := repository.NewTransferRepository()
	walletRepo := repository.NewWalletRepository()
	ledgerRepo := repository.NewLedgerRepository()

	svc := service.NewTransferService(pool, transferRepo, walletRepo, ledgerRepo)
	transferHandler := handler.NewTransferHandler(svc)
	walletHandler := handler.NewWalletHandler(svc)
	router := handler.NewRouter(transferHandler, walletHandler)

	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Recovery worker — runs every 30s. Picks up PENDING transfers older
	// than 30s and runs T2 on them. The retry handler (REPLAY) handles
	// in-flight retries inline, so this is only needed for the long-tail
	// case where the original client crashed and will never retry.
	go runRecoveryLoop(ctx, svc)

	// Start HTTP server.
	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server fatal", "error", err.Error())
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutdown initiated")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown error", "error", err.Error())
	}
	log.Info("shutdown complete")
}

// runRecoveryLoop ticks every 30s and runs one batch(default size: 100) of stale-PENDING recovery.
func runRecoveryLoop(ctx context.Context, svc *service.TransferService) {
	log := logger.Component("recovery_loop")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("recovery loop stopping")
			return
		case <-ticker.C:
			// RunRecoveryOnce logs its own outcome with detail.
			if _, err := svc.RunRecoveryOnce(ctx, 30, 100); err != nil {
				log.Warn("recovery sweep errored", "error", err.Error())
			}
		}
	}
}

// maskDSN returns just the host:port for logging the host:port.
func maskDSN(dsn string) string {
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			rest := dsn[i+1:]
			for j := 0; j < len(rest); j++ {
				if rest[j] == '/' {
					return rest[:j]
				}
			}
			return rest
		}
	}
	return "unknown"
}
