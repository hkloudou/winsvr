//go:build windows

package winsvr

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
)

// Service is the interface your program implements. Run is called once, on the
// service's own goroutine, and must block until ctx is cancelled — which
// happens when the SCM asks the service to stop or the machine shuts down.
//
// Do not call os.Exit from Run; return instead so the framework can report a
// clean stop to the SCM.
type Service interface {
	Run(ctx context.Context) error
}

// SessionAware is an optional interface. If your Service implements it, the
// framework subscribes to console/RDP session change notifications and calls
// OnSessionChange for each one (logon, logoff, lock, unlock, ...).
type SessionAware interface {
	// OnSessionChange receives the WTS_* event type. Only the event type is
	// delivered: the accompanying WTSSESSION_NOTIFICATION is freed by the OS
	// before this call runs and must not be accessed.
	OnSessionChange(eventType uint32)
}

// Run executes svc under the Service Control Manager when launched as a
// service, and on the console (Ctrl+C sends Stop) otherwise, so the same binary
// is debuggable interactively. It blocks until the service stops.
func Run(name string, service Service, stopTimeout time.Duration) error {
	if stopTimeout <= 0 {
		stopTimeout = 20 * time.Second
	}
	h := &handler{svc: service, stopTimeout: stopTimeout}
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if isSvc {
		return svc.Run(name, h)
	}
	return debug.Run(name, h)
}

type handler struct {
	svc         Service
	stopTimeout time.Duration
}

func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	accepted := svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	sessionAware, wantSession := h.svc.(SessionAware)
	if wantSession {
		accepted |= svc.AcceptSessionChange
	}

	s <- svc.Status{State: svc.StartPending, WaitHint: 15_000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.svc.Run(ctx) }()

	s <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-done:
			s <- svc.Status{State: svc.StopPending}
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.SessionChange:
				if wantSession {
					sessionAware.OnSessionChange(c.EventType)
				}
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				s <- svc.Status{State: svc.StopPending, WaitHint: uint32(h.stopTimeout / time.Millisecond)}
				cancel()
				select {
				case <-done:
				case <-time.After(h.stopTimeout):
				}
				return false, 0
			}
		}
	}
}
