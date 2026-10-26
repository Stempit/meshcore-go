package meshcore

import "log/slog"

// logger holds the package logger; nil means fall back to slog.Default().
var logger *slog.Logger

// log returns the logger used throughout the package. Defaults to
// slog.Default() (resolved per-call, so it honors a SetDefault made in main),
// tagged with module=meshcore. Override via SetLogger before connecting.
func log() *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.Default().With("module", "meshcore")
}

// SetLogger routes this package's logs to a custom logger. Call before
// connecting. Pass nil to restore the slog.Default() fallback.
func SetLogger(l *slog.Logger) { logger = l }
