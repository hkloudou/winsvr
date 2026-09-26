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

// Event IDs, so records can be filtered by severity in Event Viewer rather than
// all arriving as ID 1. EventCreate.exe, the message file Install registers,
// renders any ID from 1 to 1000.
const (
	eidInfo    = 1
	eidWarning = 2
	eidError   = 3
)

type eventLogHandler struct {
	log   *eventlog.Log
	attrs []slog.Attr
	// prefix is the open groups, already joined as "a.b.". Keys are qualified
	// when they arrive, so attributes added before a group was opened do not pick
	// up its prefix later.
	prefix string
}

func (h *eventLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *eventLogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	n := *h
	n.attrs = append([]slog.Attr{}, h.attrs...)
	for _, a := range as {
		a.Key = h.prefix + a.Key
		n.attrs = append(n.attrs, a)
	}
	return &n
}

// WithGroup qualifies the keys of later attributes. Dropping the name, as this
// used to, silently merged groups into the top level.
func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.prefix = h.prefix + name + "."
	return &n
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, " %s%s=%v", h.prefix, a.Key, a.Value); return true })
	switch msg := eventText(b.String()); {
	case r.Level >= slog.LevelError:
		return h.log.Error(eidError, msg)
	case r.Level >= slog.LevelWarn:
		return h.log.Warning(eidWarning, msg)
	default:
		return h.log.Info(eidInfo, msg)
	}
}
