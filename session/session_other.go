//go:build !windows

package session

import (
	"context"
	"errors"
	"time"
)

// ErrUnsupported is returned by every operation off Windows.
var ErrUnsupported = errors.New("session: only supported on windows")

// NoSession is the sentinel for "no active console session".
const NoSession uint32 = 0xFFFFFFFF

type Session struct {
	ID      uint32
	Station string
	State   uint32
}

type LaunchOptions struct {
	Path     string
	Args     []string
	Dir      string
	Elevated bool
	Hidden   bool
	Desktop  string
}

type Process struct{ PID uint32 }

func List() ([]Session, error)      { return nil, ErrUnsupported }
func ActiveConsole() (uint32, bool) { return NoSession, false }
func WaitForActiveConsole(ctx context.Context, poll time.Duration) (uint32, error) {
	return NoSession, ErrUnsupported
}
func LaunchInSession(sessionID uint32, o LaunchOptions) (*Process, error) {
	return nil, ErrUnsupported
}
func (p *Process) Wait(ctx context.Context) (uint32, error) { return 0, ErrUnsupported }
func (p *Process) Kill() error                              { return ErrUnsupported }
func (p *Process) Close() error                             { return nil }
func FindByPath(path string) ([]uint32, error)              { return nil, ErrUnsupported }
func KillByPath(path string) (int, error)                   { return 0, ErrUnsupported }

// Launcher is unsupported off Windows.
type Launcher struct{}

func NewLauncher() (*Launcher, error) { return nil, ErrUnsupported }
func (l *Launcher) Launch(sessionID uint32, o LaunchOptions) (*Process, error) {
	return nil, ErrUnsupported
}
func (l *Launcher) Close() error { return nil }
