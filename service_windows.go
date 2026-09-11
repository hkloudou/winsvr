//go:build windows

package winsvr

import (
	"context"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
)

// Run executes service under the SCM when launched as a service, and on the
// console (Ctrl+C = stop) otherwise, so the same binary is debuggable. It
// blocks until the service stops.
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
	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	s <- svc.Status{State: svc.StartPending, WaitHint: 15_000}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.svc.Run(ctx) }()

	s <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-done:
			// The service's Run returned on its own (not via a stop request).
			// A nil error is a clean exit; a non-nil error must not be silent —
			// log it (when the Service exposes a logger) and report a failure
			// exit code so the SCM records it and any recovery action fires.
			s <- svc.Status{State: svc.StopPending}
			if err != nil {
				if lg, ok := h.svc.(Logged); ok {
					lg.ServiceLogger().Error("service stopped: Run returned an error", "err", err)
				}
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
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

// IsWindowsService reports whether the current process is running as a Windows
// service (vs. an interactive console run). Handy for choosing a logger.
func IsWindowsService() (bool, error) { return svc.IsWindowsService() }
