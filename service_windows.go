//go:build windows

package winsvr

import (
	"context"
	"errors"
	"log/slog"
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

// log returns where framework-level events go; see serviceLogger.
func (h *handler) log() *slog.Logger { return serviceLogger(h.svc) }

func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	s <- svc.Status{State: svc.StartPending, WaitHint: 15_000}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runRecovered(ctx, h.svc) }()

	s <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case err := <-done:
			// The service's Run returned on its own, not via a stop request. A
			// nil error is a clean exit; a non-nil one must not pass in silence,
			// so log it and report a failure exit code, which is what makes the
			// SCM record it and any recovery action fire.
			s <- svc.Status{State: svc.StopPending}
			if err != nil {
				h.log().Error("service stopped: Run returned an error", "err", err)
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
				// Report a clean stop either way: a non-zero code here would have
				// the SCM's recovery action restart a service someone deliberately
				// stopped. But say what happened, because a failure passing in
				// silence is the thing Logged exists to prevent.
				select {
				case err := <-done:
					// A Service that waits on ctx.Done and returns ctx.Err is
					// doing exactly what Service documents, so that is a clean
					// stop, not a failure. Logging it would put an error in the
					// event log on every routine stop and every shutdown.
					if err != nil && !errors.Is(err, context.Canceled) {
						h.log().Error("service stopped on request, but Run returned an error", "err", err)
					}
				case <-time.After(h.stopTimeout):
					h.log().Warn("Run did not return within the stop timeout; exiting anyway",
						"timeout", h.stopTimeout.String())
				}
				return false, 0
			}
		}
	}
}

// IsWindowsService reports whether the current process is running as a Windows
// service (vs. an interactive console run). Handy for choosing a logger.
func IsWindowsService() (bool, error) { return svc.IsWindowsService() }
