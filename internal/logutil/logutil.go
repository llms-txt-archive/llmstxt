// Package logutil provides shared logging helpers.
package logutil

import "log/slog"

// Default returns l if non-nil, otherwise slog.Default().
func Default(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}
