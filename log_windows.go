//go:build windows

package winsvr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"
)

// NewEventLogger opens the Windows event-log source named source (created at
// install time by Install) and returns an *slog.Logger that writes to it, plus
// a close function. Levels map to the event log as: >=Error -> Error,
// >=Warn -> Warning, else Information.
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
	group string
}

func (h *eventLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *eventLogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), a...)
	return &nh
}

func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	nh := *h
	if nh.group != "" {
		nh.group += "." + name
	} else {
		nh.group = name
	}
	return &nh
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	writeAttr := func(a slog.Attr) {
		if h.group != "" {
			fmt.Fprintf(&b, " %s.%s=%v", h.group, a.Key, a.Value)
		} else {
			fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		}
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool { writeAttr(a); return true })
	msg := b.String()
	switch {
	case r.Level >= slog.LevelError:
		return h.log.Error(1, msg)
	case r.Level >= slog.LevelWarn:
		return h.log.Warning(1, msg)
	default:
		return h.log.Info(1, msg)
	}
}
