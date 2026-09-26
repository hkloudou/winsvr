// Package winsvr turns a Go program into a Windows service, and provides a
// ready-made Supervisor that keeps a payload binary updated from a URL and runs
// it in the logged-on user's desktop session.
//
// See the README for the service/helper split. In short: the service (this
// package, running as LocalSystem) contains no business logic — it installs
// itself, updates the payload, and launches it in the user's session. The
// payload is where your real program lives, running as the interactive user.
//
// Every Windows-only call has a non-Windows stub returning ErrUnsupported, so a
// project using this package still builds and unit-tests on Linux and macOS.
package winsvr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// ErrUnsupported is returned by Windows-only calls on other platforms.
var ErrUnsupported = errors.New("winsvr: only supported on windows")

// ErrNoUserSession reports that no user is signed in to the target session, so
// there is nothing to launch into. A logon screen and a signed-out machine both
// look like this. It is a normal waiting condition rather than a failure, so
// callers should wait for a sign-in instead of retrying harder.
var ErrNoUserSession = errors.New("winsvr: no user signed in to the session")

// ErrNoElevatedToken reports that an elevated launch was asked for but the user
// signed in to the session is not an administrator, so no elevated token exists
// to launch with. Unlike ErrNoUserSession this will not resolve by waiting: it
// means LaunchElevated is set on a machine whose user cannot satisfy it, and it
// is reported rather than quietly launching a payload without the rights it was
// configured to need.
var ErrNoElevatedToken = errors.New("winsvr: the session's user has no elevated token")

// ErrElevationRequired reports that the payload's manifest asks for
// administrator, so a filtered token cannot start it at all: Windows refuses
// with ERROR_ELEVATION_REQUIRED before the process exists. Supervisor treats it
// as the signal to retry elevated, unless DisableAutoElevate says not to.
var ErrElevationRequired = errors.New("winsvr: the payload requires administrator")

// Logged is an optional interface a Service may implement so winsvr.Run can log
// framework-level events to the service's own logger — most importantly, the
// error returned when Run exits unexpectedly, which would otherwise be silent.
// Supervisor implements it.
type Logged interface {
	ServiceLogger() *slog.Logger
}

// Bounds on what a recovered panic contributes to its error. Together they stay
// well inside one event-log record. The value gets its own, smaller share so
// that a huge one cannot push the stack out entirely: the stack is what says
// where.
const (
	maxPanicValue = 4 << 10
	maxPanicStack = 16 << 10
)

// maxEventLen keeps one event-log record under ReportEvent's limit of 31,839
// characters per string. It counts bytes, which are never fewer than the UTF-16
// units Windows counts, so it is safe for any text.
const maxEventLen = 31000

// truncate shortens s to at most n bytes, cutting on a character boundary and
// saying that it did.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + " …(truncated)"
}

// eventText makes a message safe to hand to the event log. Both problems it
// handles would otherwise lose the line silently, because ReportEvent's failure
// comes back as a handler error and slog discards that: a NUL inside the text
// fails the conversion to a NUL-terminated string, and an over-long one is
// refused outright. A NUL is shown rather than dropped, since its presence is
// usually the interesting part.
func eventText(s string) string {
	return truncate(strings.ReplaceAll(s, "\x00", `\x00`), maxEventLen)
}

// runRecovered calls svc.Run and turns a panic into an error.
//
// A service's Run executes on a goroutine of its own, where a panic ends the
// whole process, and under the service control manager stderr is connected to
// nothing. So the cause would be lost entirely, leaving only "terminated
// unexpectedly" in the system log. As an error it is logged instead, and
// reported as a failed exit, so the recovery action fires and someone can see
// why. It also covers a nil Service, whose Run would panic the same way.
func runRecovered(ctx context.Context, svc Service) (err error) {
	defer func() {
		if r := recover(); r != nil {
			value := truncate(fmt.Sprint(r), maxPanicValue)
			stack := truncate(string(debug.Stack()), maxPanicStack)
			err = fmt.Errorf("winsvr: the service panicked: %s\n%s", value, stack)
		}
	}()
	return svc.Run(ctx)
}

// serviceLogger returns the logger framework events about svc should go to.
//
// That is svc's own logger when it has one that will actually record an error.
// Otherwise it is stderr, never a logger that discards: a recovered panic is
// written here, and discarding it would make it vanish. Checking only for the
// Logged interface is not enough, because a Supervisor with no Logger set
// implements it and still discards. In console mode stderr is the terminal,
// which is where an unrecovered panic used to appear; as a service it is
// connected to nothing, which is no worse than before.
func serviceLogger(svc Service) *slog.Logger {
	if lg, ok := svc.(Logged); ok {
		if l := lg.ServiceLogger(); l != nil && l.Enabled(context.Background(), slog.LevelError) {
			return l
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// Service is what you implement. Run is called once and must block until ctx is
// cancelled — which happens when the service is asked to stop or the machine
// shuts down. Do not call os.Exit; return instead.
type Service interface {
	Run(ctx context.Context) error
}

// StartType controls when the SCM starts the service.
type StartType int

const (
	StartAutomatic StartType = iota // start on boot (zero value)
	StartManual                     // start only on request
	StartDisabled                   // cannot be started
)

// Config describes the service to the Windows Service Control Manager.
type Config struct {
	Name         string        // service key name, no spaces (required)
	DisplayName  string        // shown in services.msc (defaults to Name)
	Description  string        // long description
	Arguments    []string      // appended to the service command line at install
	StartType    StartType     // when to start (default: automatic)
	Account      string        // log-on account; must be LocalSystem to enter a user session
	Password     string        // account password, if any
	Dependencies []string      // services that must start first
	RestartDelay time.Duration // SCM auto-restart delay after a crash (0 = none)
	StopTimeout  time.Duration // how long Run waits for a clean stop (default 20s)
}

func (c *Config) displayName() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

// ServiceState is the run state of a service (mirrors the SCM states).
type ServiceState uint32

const (
	Stopped         ServiceState = 1
	StartPending    ServiceState = 2
	StopPending     ServiceState = 3
	Running         ServiceState = 4
	ContinuePending ServiceState = 5
	PausePending    ServiceState = 6
	Paused          ServiceState = 7
)

func (s ServiceState) String() string {
	switch s {
	case Stopped:
		return "stopped"
	case StartPending:
		return "start-pending"
	case StopPending:
		return "stop-pending"
	case Running:
		return "running"
	case ContinuePending:
		return "continue-pending"
	case PausePending:
		return "pause-pending"
	case Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown(%d)", uint32(s))
	}
}
