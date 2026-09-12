//go:build !windows

package winsvr

import (
	"context"
	"time"
)

type LaunchOptions struct {
	Path   string
	Args   []string
	Dir    string
	Hidden bool
}

type Process struct{ PID uint32 }

func ActiveConsoleSession() (uint32, bool) { return 0xFFFFFFFF, false }
func WaitForActiveConsole(ctx context.Context, poll time.Duration) (uint32, error) {
	return 0xFFFFFFFF, ErrUnsupported
}
func LaunchInSession(sessionID uint32, o LaunchOptions) (*Process, error) { return nil, ErrUnsupported }
func (p *Process) Wait(ctx context.Context) (uint32, error)               { return 0, ErrUnsupported }
func (p *Process) Close() error                                           { return nil }
