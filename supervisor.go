package winsvr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Supervisor is a ready-made Service that, each time the service starts, brings
// a payload binary up to date from a URL and then runs it inside the logged-on
// user's desktop session, restarting it if it crashes and killing it when the
// service stops.
//
// Update timing: the check runs once per service start (i.e. once per boot for
// an auto-start service, and again on every SCM restart), and always *before*
// the payload is launched. It waits for the network: the check is retried until
// it succeeds, so a machine that boots offline does not run a possibly-stale
// payload — it waits until connectivity returns, confirms the payload is
// current, and only then launches it. Crash-restarts of the payload do not
// re-check; a fresh check happens the next time the service itself starts.
type Supervisor struct {
	// Bin is the payload file name, resolved next to the service executable
	// (e.g. "helper.bin"). Required.
	Bin string
	// UpdateURL is the remote payload. Empty disables updates (the local Bin is
	// launched directly, with no network wait).
	UpdateURL string
	// Sidecar checks <UpdateURL>.json (version + crc64) instead of HEAD/ETag.
	Sidecar bool
	// Elevated launches the payload with the user's elevated token (no UAC
	// prompt, since the service is LocalSystem). A "requireAdministrator"
	// payload requires this; an "asInvoker" payload should leave it false.
	Elevated bool
	// Hidden launches the payload without a console window. Recommended; also
	// build the payload with `-ldflags -H windowsgui`.
	Hidden bool
	// MinBackoff/MaxBackoff bound the exponential restart delay (default 2s/30s).
	MinBackoff, MaxBackoff time.Duration
	// Logger receives progress logs. Defaults to no-op; wire NewEventLogger for
	// a service, or a stderr slog handler for interactive debugging.
	Logger *slog.Logger

	dir string
}

func (s *Supervisor) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(discardHandler{})
}

// ServiceLogger implements Logged, so winsvr.Run logs framework events (such as
// Run returning an error) to the Supervisor's logger.
func (s *Supervisor) ServiceLogger() *slog.Logger { return s.log() }

var _ Logged = (*Supervisor)(nil)

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
	log := s.log()
	if s.Bin == "" {
		err := fmt.Errorf("winsvr: Supervisor.Bin is required")
		log.Error("cannot start service", "err", err)
		return err
	}
	bin, err := s.binPath()
	if err != nil {
		log.Error("cannot resolve payload path", "err", err)
		return err
	}
	log.Info("service starting",
		"payload", bin,
		"updateURL", s.UpdateURL,
		"sidecar", s.Sidecar,
		"elevated", s.Elevated,
		"hidden", s.Hidden)

	// 1. Update check — before the payload runs, and only after the network is
	//    up (retried until it succeeds).
	if s.UpdateURL != "" {
		if err := s.updateBeforeLaunch(ctx, bin); err != nil {
			log.Info("service stopping before first launch", "reason", err)
			return nil // ctx cancelled while waiting for the network
		}
	} else {
		log.Info("auto-update disabled; launching existing payload")
	}
	if _, err := os.Stat(bin); err != nil {
		e := fmt.Errorf("winsvr: payload %q not found: %w", bin, err)
		log.Error("payload not found; service will exit", "path", bin, "err", err)
		return e
	}

	// 2. Launch and supervise. No further update checks until the next service
	//    start.
	min, max := s.MinBackoff, s.MaxBackoff
	if min <= 0 {
		min = 2 * time.Second
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	if max < min {
		max = min
	}
	backoff := min
	for ctx.Err() == nil {
		log.Debug("waiting for an active user session")
		sid, err := WaitForActiveConsole(ctx, 2*time.Second)
		if err != nil {
			log.Info("service stopping", "reason", err)
			return nil
		}
		log.Info("launching payload", "session", sid, "path", bin)
		proc, err := LaunchInSession(sid, LaunchOptions{Path: bin, Elevated: s.Elevated, Hidden: s.Hidden})
		if err != nil {
			log.Error("launch failed; will retry", "err", err, "backoff", backoff.String())
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = grow(backoff, max)
			continue
		}
		log.Info("payload running", "pid", proc.PID, "session", sid)

		started := time.Now()
		code, werr := proc.Wait(ctx)
		proc.Close() // terminates the payload process tree via the job object
		if ctx.Err() != nil {
			log.Info("service stopping; payload terminated", "pid", proc.PID)
			return nil
		}
		// Reset the backoff only after the payload stayed up long enough to be
		// considered healthy. A payload that starts but crashes immediately must
		// keep backing off, or it would restart-storm at the minimum interval.
		if time.Since(started) >= max {
			backoff = min
		}
		if werr != nil {
			log.Warn("payload wait error; will restart", "pid", proc.PID, "err", werr, "backoff", backoff.String())
		} else {
			log.Warn("payload exited; will restart", "pid", proc.PID, "code", code, "backoff", backoff.String())
		}
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = grow(backoff, max)
	}
	return nil
}

// updateBeforeLaunch runs the update check and installs a newer payload, waiting
// for the network by retrying until the check succeeds or ctx is cancelled.
func (s *Supervisor) updateBeforeLaunch(ctx context.Context, bin string) error {
	log := s.log()
	// Pass the logger so the Updater logs the ETag/version comparison and the
	// resulting action ("update check ... needsUpdate=... action=...").
	up := &Updater{URL: s.UpdateURL, Path: bin, Sidecar: s.Sidecar, Logger: log}

	const maxWait = 60 * time.Second
	wait := 2 * time.Second
	for attempt := 1; ctx.Err() == nil; attempt++ {
		log.Debug("checking for payload update", "attempt", attempt, "url", s.UpdateURL)
		_, err := up.EnsureLatest(ctx)
		if err == nil {
			return nil // the Updater already logged the comparison and action
		}
		if ctx.Err() != nil {
			return ctx.Err() // the service is stopping, not an update failure
		}
		// Most commonly this is "no network yet"; keep waiting. Config errors
		// (bad URL, 404) also land here and repeat in the log so they are easy
		// to spot while debugging.
		log.Warn("update check failed; waiting for network", "attempt", attempt, "err", err, "retryIn", wait.String())
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
		wait = grow(wait, maxWait)
	}
	return ctx.Err()
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
