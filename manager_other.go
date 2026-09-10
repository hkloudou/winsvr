//go:build !windows

package winsvr

import "context"

func Install(c Config) error                   { return ErrUnsupported }
func Uninstall(name string) error              { return ErrUnsupported }
func Status(name string) (ServiceState, error) { return 0, ErrUnsupported }
func Start(name string, args ...string) error  { return ErrUnsupported }
func Stop(name string) error                   { return ErrUnsupported }
func IsElevated() bool                         { return false }
func Elevate(ctx context.Context, args []string) (uint32, error) {
	return 0, ErrUnsupported
}
