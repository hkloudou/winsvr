package winsvr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hkloudou/winsvr/session"
	"github.com/hkloudou/winsvr/update"
)

// RunPolicy controls how and where the payload binary is launched.
type RunPolicy struct {
	// Args are passed to the payload.
	Args []string
	// Elevated launches the payload with the user's linked admin token (no UAC
	// prompt, since the service runs as LocalSystem). Requires the payload to
	// be manifested "asInvoker" or "requireAdministrator"; a "requireAdministrator"
	// payload MUST set Elevated, or it cannot start.
	Elevated bool
	// Hidden launches the payload with no console window. Recommended for
	// background helpers; also build the payload with `-ldflags -H windowsgui`.
	Hidden bool
	// MinBackoff and MaxBackoff bound the exponential delay between payload
	// restarts. Default 2s..30s.
	MinBackoff, MaxBackoff time.Duration
	// UpdateInterval, when > 0 and an update Source is set, polls for a newer
	// payload while it runs (cheap HEAD/sidecar check). When a newer version is
	// detected the running payload is stopped, the update installed, and the
	// new payload launched. When 0, updates are only applied between runs
	// (before each launch and after each exit).
	UpdateInterval time.Duration
	// RequireUpdate makes the supervisor refuse to launch a payload it could
	// not update-check when no local copy exists yet. By default an offline
	// check is tolerated and the existing payload runs, so boot is not blocked
	// on the network.
	RequireUpdate bool
}

func (p RunPolicy) minBackoff() time.Duration {
	if p.MinBackoff > 0 {
		return p.MinBackoff
	}
	return 2 * time.Second
}

func (p RunPolicy) maxBackoff() time.Duration {
	if p.MaxBackoff > 0 {
		return p.MaxBackoff
	}
	return 30 * time.Second
}

// Supervisor is a ready-made Service that keeps a payload binary updated from a
// remote URL and runs it inside the logged-on user's session, restarting it on
// exit and killing it (and its whole process tree) when the service stops.
//
// It implements Service, so run it with Run(cfg.Name, sup, cfg.StopTimeout) or,
// more simply, build an Agent with NewAgent and call Agent.Main.
type Supervisor struct {
	// Bin is the payload file name, resolved relative to the service
	// executable's directory (e.g. "helper.bin"). Required.
	Bin string
	// Update, when non-nil, keeps Bin in sync with a remote copy.
	Update *update.Source
	// Policy controls how the payload is launched.
	Policy RunPolicy
	// Logger receives structured progress logs. Defaults to a logger writing to
	// the Windows event log when running as a service (wired by Agent), else a
	// no-op.
	Logger *slog.Logger

	dir string
}

func (s *Supervisor) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(discardHandler{})
}

// BinPath returns the absolute path of the payload binary.
func (s *Supervisor) BinPath() (string, error) {
	if s.dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		s.dir = filepath.Dir(exe)
	}
	return filepath.Join(s.dir, s.Bin), nil
}

func (s *Supervisor) updater(binPath string) *update.Updater {
	if s.Update == nil {
		return nil
	}
	return &update.Updater{Source: *s.Update, Path: binPath}
}

// Run implements Service. It blocks until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.Bin == "" {
		return errors.New("winsvr: Supervisor.Bin is required")
	}
	binPath, err := s.BinPath()
	if err != nil {
		return err
	}
	launcher, err := session.NewLauncher()
	if err != nil {
		return fmt.Errorf("create launcher: %w", err)
	}
	defer launcher.Close() // kills the payload tree on service stop

	up := s.updater(binPath)
	backoff := s.Policy.minBackoff()

	for ctx.Err() == nil {
		// Update between runs: the payload is not running, so replacing the
		// file on disk is always safe here.
		if up != nil {
			if err := s.tryUpdate(ctx, up); err != nil {
				if !fileExists(binPath) && s.Policy.RequireUpdate {
					s.log().Warn("no payload yet and update failed; retrying", "err", err)
					if !sleep(ctx, backoff) {
						return ctx.Err()
					}
					backoff = nextBackoff(backoff, s.Policy.maxBackoff())
					continue
				}
				s.log().Warn("update check failed; using existing payload", "err", err)
			}
		}
		if !fileExists(binPath) {
			return fmt.Errorf("winsvr: payload %q not found and no update source produced it", binPath)
		}

		restart, err := s.superviseOnce(ctx, launcher, up, binPath)
		if err != nil && ctx.Err() == nil {
			s.log().Error("payload supervision error", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if restart {
			backoff = s.Policy.minBackoff() // an update or clean relaunch resets backoff
		} else {
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, s.Policy.maxBackoff())
		}
	}
	return nil
}

// superviseOnce launches the payload once and waits for it to exit, the service
// to stop, or (when polling) an update to become available. It returns
// restart=true when the loop should relaunch promptly (update pending), and
// false when the payload exited on its own (apply backoff).
func (s *Supervisor) superviseOnce(ctx context.Context, l *session.Launcher, up *update.Updater, binPath string) (restart bool, err error) {
	sid, err := session.WaitForActiveConsole(ctx, 2*time.Second)
	if err != nil {
		return false, err
	}
	proc, err := l.Launch(sid, session.LaunchOptions{
		Path:     binPath,
		Args:     s.Policy.Args,
		Elevated: s.Policy.Elevated,
		Hidden:   s.Policy.Hidden,
	})
	if err != nil {
		return false, err
	}
	s.log().Info("payload started", "pid", proc.PID, "session", sid, "path", binPath)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	updatePending := false
	if up != nil && s.Policy.UpdateInterval > 0 {
		go func() {
			t := time.NewTicker(s.Policy.UpdateInterval)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					if ok, err := up.Available(runCtx); err == nil && ok {
						s.log().Info("newer payload available; recycling")
						updatePending = true
						cancel()
						return
					}
				}
			}
		}()
	}

	code, werr := proc.Wait(runCtx)
	if werr != nil { // ctx cancelled: service stopping or update pending
		_ = proc.Kill()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = proc.Wait(stopCtx)
		stopCancel()
		proc.Close()
		if ctx.Err() != nil {
			return false, nil
		}
		return updatePending, nil // relaunch promptly if an update is pending
	}
	proc.Close()
	s.log().Info("payload exited", "pid", proc.PID, "code", code)
	return false, nil // exited on its own -> backoff then relaunch
}

func (s *Supervisor) tryUpdate(ctx context.Context, up *update.Updater) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := up.EnsureLatest(cctx)
	if err != nil {
		return err
	}
	if res.Updated {
		s.log().Info("payload updated", "version", res.State.Version, "etag", res.State.ETag)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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

func nextBackoff(cur, max time.Duration) time.Duration {
	cur *= 2
	if cur > max {
		return max
	}
	return cur
}

// discardHandler is a slog.Handler that drops everything.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
