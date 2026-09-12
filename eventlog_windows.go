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
	log    *eventlog.Log
	attrs  []slog.Attr
	groups []string
}

func (h *eventLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

// qualify prefixes a key with the open groups, so a key "k" inside group "g"
// becomes "g.k".
func (h *eventLogHandler) qualify(key string) string {
	if len(h.groups) == 0 {
		return key
	}
	return strings.Join(h.groups, ".") + "." + key
}

func (h *eventLogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	n := *h
	n.attrs = append([]slog.Attr{}, h.attrs...)
	// Resolve each key against the groups open right now. Attributes added
	// before a group was opened must not pick up its prefix later.
	for _, a := range as {
		a.Key = h.qualify(a.Key)
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
	n.groups = append(append([]string{}, h.groups...), name)
	return &n
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, " %s=%v", h.qualify(a.Key), a.Value); return true })
	switch msg := b.String(); {
	case r.Level >= slog.LevelError:
		return h.log.Error(eidError, msg)
	case r.Level >= slog.LevelWarn:
		return h.log.Warning(eidWarning, msg)
	default:
		return h.log.Info(eidInfo, msg)
	}
}
