package winsvr

import (
	"fmt"
	"time"
)

// StartType controls when the service is started by the Service Control
// Manager. The zero value (StartAutomatic) auto-starts on boot.
type StartType int

const (
	// StartAutomatic starts the service on boot. This is the zero value so a
	// freshly declared Config auto-starts, which is what most agents want.
	StartAutomatic StartType = iota
	// StartManual requires the service to be started explicitly.
	StartManual
	// StartDisabled prevents the service from being started at all.
	StartDisabled
)

// Config describes a Windows service to the Service Control Manager. It is a
// plain data type and is safe to construct on any platform.
type Config struct {
	// Name is the service key name registered with the SCM. No spaces.
	// Required.
	Name string
	// DisplayName is shown in services.msc. Defaults to Name.
	DisplayName string
	// Description is the long description shown in services.msc.
	Description string
	// Arguments are appended to the service's command line at install time,
	// so the running service can tell it was launched by the SCM.
	Arguments []string
	// StartType controls when the SCM starts the service.
	StartType StartType
	// DelayedAutoStart delays an automatic start until after other auto-start
	// services, reducing boot contention. Ignored unless StartAutomatic.
	DelayedAutoStart bool
	// Dependencies lists other services that must start first.
	Dependencies []string
	// Account is the log-on account, e.g. "NT AUTHORITY\\LocalSystem" (the
	// default when empty) or ".\\Administrator". LocalSystem is required to
	// launch processes into another user's session.
	Account string
	// Password is the account password, if Account requires one.
	Password string
	// RestartDelay, when > 0, configures SCM recovery so the service is
	// restarted this long after an unexpected failure.
	RestartDelay time.Duration
	// StopTimeout bounds how long Run waits for your Service to return after a
	// Stop/Shutdown before reporting stopped anyway. Defaults to 20s.
	StopTimeout time.Duration
}

func (c *Config) displayName() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

func (c *Config) stopTimeout() time.Duration {
	if c.StopTimeout > 0 {
		return c.StopTimeout
	}
	return 20 * time.Second
}

// ServiceState is the current run state of a service, mirroring the SCM states.
type ServiceState uint32

// Service states, matching golang.org/x/sys/windows/svc.State values.
const (
	Stopped         ServiceState = 1
	StartPending    ServiceState = 2
	StopPending     ServiceState = 3
	Running         ServiceState = 4
	ContinuePending ServiceState = 5
	PausePending    ServiceState = 6
	Paused          ServiceState = 7
)

// String renders the state name.
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
