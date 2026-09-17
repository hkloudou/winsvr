package winsvr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	// LaunchElevated launches the payload with the signed-in user's elevated
	// token, so it runs at high integrity. Default false, which is the right
	// answer unless you know you need it.
	//
	// Two things need it. A payload whose manifest says requireAdministrator
	// cannot start from a filtered token at all, and fails with
	// ERROR_ELEVATION_REQUIRED. And desktop automation against an application
	// that is itself running as administrator is blocked by UIPI, which stops a
	// medium-integrity process from reaching a high-integrity window: messages,
	// hooks and UI Automation all fail, and they fail silently, so the symptom
	// is an action that simply does not happen.
	//
	// No UAC prompt appears. The service is LocalSystem and takes the token
	// rather than asking for consent, which is also why this works with nobody
	// watching the screen.
	//
	// Know what it costs. The payload gets administrator rights, so a flaw in it
	// is an administrator-level flaw. Files and registry keys it creates carry
	// high integrity, which means the user cannot later modify them from an
	// ordinary process, and that surprises people. It is not a security
	// boundary either: the process still runs inside the user's own session,
	// where that user can debug it.
	//
	// If the signed-in user is not an administrator there is no elevated token
	// to use. That is reported as ErrNoElevatedToken and the service exits,
	// rather than launching a payload without the rights it was configured to
	// need.
	LaunchElevated bool
	// DisableAutoElevate turns off the retry described here. It is named in the
	// negative because the retry is on by default and Go's zero value is false;
	// the same shape as http.Transport.DisableKeepAlives.
	//
	// By default a payload whose manifest says requireAdministrator is launched
	// elevated even when LaunchElevated is not set. Such a payload cannot start
	// from a filtered token at all, so the first attempt fails with
	// ERROR_ELEVATION_REQUIRED and is retried once with the elevated token. The
	// answer is remembered for the rest of the run, so later restarts go
	// straight there, and the first time it happens is logged.
	//
	// Understand what that delegates. The payload arrives over a channel nothing
	// authenticates, so its manifest, and in effect whoever controls the URL it
	// came from, decides whether it runs as administrator. Set this to keep that
	// decision here, where LaunchElevated alone then answers it.
	DisableAutoElevate bool
	// Hidden launches the payload without a console window. Recommended; also
	// build the payload with `-ldflags -H windowsgui`.
	Hidden bool
	// MinBackoff/MaxBackoff bound the exponential restart delay (default 2s/30s).
	MinBackoff, MaxBackoff time.Duration
	// HTTPClient overrides the client used for the update check and download.
	// The default allows five minutes for one whole request, body included, and
	// refuses a redirect that drops TLS. Supply your own for a large payload on
	// a slow link, with no Timeout, so only the context bounds the transfer.
	HTTPClient *http.Client
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
		"elevated", s.LaunchElevated,
		"autoElevate", !s.DisableAutoElevate,
		"hidden", s.Hidden)

	// 1. Update check — before the payload runs, and only after the network is
	//    up (retried until it succeeds).
	if s.UpdateURL != "" {
		if err := s.updateBeforeLaunch(ctx, bin); err != nil {
			// Today this only ever reports cancellation, because the check is
			// retried until it passes. Do not assume that: reporting an
			// unexpected failure as a clean stop would hide it from the SCM,
			// which then has no reason to restart the service.
			if ctx.Err() == nil {
				log.Error("update check gave up; service will exit", "err", err)
				return err
			}
			log.Info("service stopping before first launch", "reason", err)
			return nil
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
	// Whether to launch elevated. It starts at the configured value and can be
	// raised once, by the auto-elevate retry below, which is what keeps every
	// later restart from failing the same way first.
	elevate := s.LaunchElevated
	backoff := min
	for ctx.Err() == nil {
		log.Debug("waiting for an active user session")
		sid, err := WaitForActiveConsole(ctx, 2*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				// Not a stop, so something is wrong with session lookup itself.
				// Returning nil would report a clean stop and the SCM would
				// leave the service down.
				log.Error("cannot wait for a user session; service will exit", "err", err)
				return fmt.Errorf("winsvr: waiting for a user session: %w", err)
			}
			log.Info("service stopping", "reason", err)
			return nil
		}
		log.Info("launching payload", "session", sid, "path", bin)
		proc, err := LaunchInSession(sid, LaunchOptions{Path: bin, Elevated: elevate, Hidden: s.Hidden})
		if err != nil {
			// An elevated launch was needed but this user cannot supply one.
			// Waiting will not change that, and a backoff would bury a
			// configuration mistake under a retry loop, so exit and let the SCM
			// record it.
			if errors.Is(err, ErrNoElevatedToken) {
				log.Error("cannot launch elevated; service will exit", "session", sid, "err", err)
				return err
			}
			if shouldAutoElevate(err, elevate, s.DisableAutoElevate) {
				// Logged at warning level on purpose: the payload is about to
				// run as administrator without anyone having configured that.
				log.Warn("the payload's manifest requires administrator; launching it elevated",
					"path", bin,
					"note", "set LaunchElevated to make this explicit, or DisableAutoElevate to refuse it")
				elevate = true
				continue
			}
			// A user who signed out between the session check and the launch is
			// not a failure: wait again rather than logging an error and widening
			// the backoff, which at a logon screen would repeat indefinitely.
			if errors.Is(err, ErrNoUserSession) {
				log.Info("no interactive user yet; waiting for sign-in", "session", sid)
				if !sleep(ctx, min) {
					return nil
				}
				continue
			}
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
		if time.Since(started) >= healthyAfter {
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

// updateBeforeLaunch runs the update check and installs a newer payload, retrying
// until it succeeds or ctx is cancelled. That covers waiting for the network and
// equally a server that sends no ETag, and so cannot identify its payload: it
// does not launch until the check passes.
func (s *Supervisor) updateBeforeLaunch(ctx context.Context, bin string) error {
	log := s.log()
	// Pass the logger so the Updater logs the ETag/version comparison and the
	// resulting action ("update check ... needsUpdate=... action=...").
	up := &Updater{URL: s.UpdateURL, Path: bin, Client: s.HTTPClient, Logger: log}

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
		// Usually "no network yet", so keep waiting. A bad URL, a 404 and a
		// missing ETag land here too and repeat in the log rather than being
		// worked around: the payload has to be identifiable before it runs.
		log.Warn("update check failed; will retry", "attempt", attempt, "err", err, "retryIn", wait.String())
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
		wait = grow(wait, maxWait)
	}
	return ctx.Err()
}

// healthyAfter is how long the payload must stay up before the restart backoff
// resets to MinBackoff. It is deliberately not MaxBackoff, which bounds the pause
// between retries and says nothing about how long a run must last to count.
const healthyAfter = 30 * time.Second

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
