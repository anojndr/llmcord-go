package providers

import "log/slog"

// logWarn logs a warning with an error and attributes.
func logWarn(message string, err error, attrs ...any) {
	slog.Warn(message, append([]any{"error", err}, attrs...)...)
}
