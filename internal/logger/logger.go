// Package logger provides a centralised, structured logger built on log/slog.

package logger

import (
	"log/slog"
	"os"
	"strings"
)

// Init configures the package-level default logger from environment.
// Call this once from main() before anything else logs.
// Env vars:
//
//	WALLET_LOG_LEVEL  — "debug" | "info" (default) | "warn" | "error"
//	WALLET_LOG_FORMAT — "json" (default) | "text"
func Init() {
	level := parseLevel(os.Getenv("WALLET_LOG_LEVEL"))
	format := strings.ToLower(os.Getenv("WALLET_LOG_FORMAT"))

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch format {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// Component returns a logger tagged with the given component/layer name.
// Use at the entry of a layer or function:
// Filtering by `component` in log aggregators groups logs by layer.
func Component(name string) *slog.Logger {
	return slog.Default().With("component", name)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
