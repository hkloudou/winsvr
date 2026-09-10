//go:build !windows

package winsvr

import "log/slog"

// NewEventLogger returns a no-op logger off Windows.
func NewEventLogger(source string) (*slog.Logger, func() error, error) {
	return slog.New(discardHandler{}), func() error { return nil }, nil
}
