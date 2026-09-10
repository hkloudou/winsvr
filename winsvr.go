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
	"time"
)

// ErrUnsupported is returned by Windows-only calls on other platforms.
var ErrUnsupported = errors.New("winsvr: only supported on windows")

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
