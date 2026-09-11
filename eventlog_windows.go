//go:build windows

package winsvr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"
)

// NewEventLogger opens the event-log source created by Install and returns an
// *slog.Logger writing to it, plus a close func. Levels map to Error/Warning/
// Information. A service has no stdout, so this is the natural place to log.
func NewEventLogger(source string) (*slog.Logger, func() error, error) {
	l, err := eventlog.Open(source)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(&eventLogHandler{log: l}), l.Close, nil
}

type eventLogHandler struct {
	log   *eventlog.Log
	attrs []slog.Attr
}

func (h *eventLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }
func (h *eventLogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &eventLogHandler{log: h.log, attrs: append(append([]slog.Attr{}, h.attrs...), a...)}
}
func (h *eventLogHandler) WithGroup(string) slog.Handler { return h }
func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, " %s=%v", a.Key, a.Value); return true })
	switch msg := b.String(); {
	case r.Level >= slog.LevelError:
		return h.log.Error(1, msg)
	case r.Level >= slog.LevelWarn:
		return h.log.Warning(1, msg)
	default:
		return h.log.Info(1, msg)
	}
}
