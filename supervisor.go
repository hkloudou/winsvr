package winsvr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Supervisor is a ready-made Service that, on startup, brings a payload binary
// up to date from a URL (a single check — see below), then runs it inside the
// logged-on user's desktop session, restarting it if it crashes and killing it
// when the service stops.
//
// Update policy: the check happens once, when the service starts (i.e. once per
// boot for an auto-start service). If a newer payload exists it is downloaded
// and installed before the payload is launched, so the payload that runs is
// always the current one. Crash-restarts do not re-check. If the check fails
// (offline) the existing payload is used, so startup is never blocked on the
// network.
type Supervisor struct {
	// Bin is the payload file name, resolved next to the service executable
	// (e.g. "helper.bin"). Required.
	Bin string
	// UpdateURL is the remote payload. Empty disables updates.
	UpdateURL string
	// Sidecar checks <UpdateURL>.json (version + crc64) instead of HEAD/ETag.
	Sidecar bool
	// Elevated launches the payload with the user's elevated token (no UAC
	// prompt, since the service is LocalSystem). The payload must be manifested
	// "asInvoker" or "requireAdministrator"; a "requireAdministrator" payload
	// requires this to be true.
	Elevated bool
	// Hidden launches the payload without a console window. Recommended; also
	// build the payload with `-ldflags -H windowsgui`.
	Hidden bool
	// MinBackoff/MaxBackoff bound the exponential restart delay (default 2s/30s).
	MinBackoff, MaxBackoff time.Duration
	// Logger receives progress logs. Defaults to no-op; wire NewEventLogger.
	Logger *slog.Logger

	dir string
}

func (s *Supervisor) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(discardHandler{})
}

func (s *Supervisor) binPath() (string, error) {
	if s.dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		s.dir = filepath.Dir(exe)
	}
	return filepath.Join(s.dir, s.Bin), nil
}

// Run implements Service.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.Bin == "" {
		return fmt.Errorf("winsvr: Supervisor.Bin is required")
	}
	bin, err := s.binPath()
	if err != nil {
		return err
	}

	// 1. One update check at startup, while the payload is stopped (so replacing
	//    the file is safe). Best-effort: an offline check falls back to the
	//    existing payload.
	if s.UpdateURL != "" {
		up := &Updater{URL: s.UpdateURL, Path: bin, Sidecar: s.Sidecar}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		updated, err := up.EnsureLatest(cctx)
		cancel()
		switch {
		case err != nil:
			s.log().Warn("update check failed; using existing payload", "err", err)
		case updated:
			s.log().Info("payload updated", "path", bin)
		default:
			s.log().Info("payload is up to date")
		}
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("winsvr: payload %q not found and no update produced it", bin)
	}

	// 2. Launch and supervise. No further update checks.
	min, max := s.MinBackoff, s.MaxBackoff
	if min <= 0 {
		min = 2 * time.Second
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	backoff := min
	for ctx.Err() == nil {
		sid, err := WaitForActiveConsole(ctx, 2*time.Second)
		if err != nil {
			return nil // ctx cancelled
		}
		proc, err := LaunchInSession(sid, LaunchOptions{Path: bin, Elevated: s.Elevated, Hidden: s.Hidden})
		if err != nil {
			s.log().Error("launch failed", "err", err)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = grow(backoff, max)
			continue
		}
		s.log().Info("payload started", "pid", proc.PID, "session", sid)
		code, werr := proc.Wait(ctx)
		proc.Close() // kills the payload tree
		if ctx.Err() != nil {
			return nil
		}
		if werr != nil {
			s.log().Warn("payload wait error", "err", werr)
		} else {
			s.log().Info("payload exited", "code", code)
		}
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = grow(backoff, max)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func grow(cur, max time.Duration) time.Duration {
	if cur *= 2; cur > max {
		return max
	}
	return cur
}

// discardHandler drops all log records.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
