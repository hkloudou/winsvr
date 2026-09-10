//go:build !windows

package winsvr

import "time"

// Run is unsupported off Windows.
func Run(name string, service Service, stopTimeout time.Duration) error { return ErrUnsupported }
