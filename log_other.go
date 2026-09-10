//go:build !windows

package winsvr

import "log/slog"

// NewEventLogger is unsupported off Windows and returns a no-op logger.
func NewEventLogger(source string) (*slog.Logger, func() error, error) {
	return slog.New(discardHandler{}), func() error { return nil }, nil
}
