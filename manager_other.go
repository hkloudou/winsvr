//go:build !windows

package winsvr

func Install(c Config) error                   { return ErrUnsupported }
func Uninstall(name string) error              { return ErrUnsupported }
func Status(name string) (ServiceState, error) { return 0, ErrUnsupported }
func Start(name string, args ...string) error  { return ErrUnsupported }
func Stop(name string) error                   { return ErrUnsupported }
