//go:build !windows

package winsvr

import (
	"context"
	"time"
)

// Service is the interface your program implements. See the Windows build for
// the full contract.
type Service interface {
	Run(ctx context.Context) error
}

// SessionAware is an optional interface implemented by services that want
// console/RDP session change notifications.
type SessionAware interface {
	OnSessionChange(eventType uint32)
}

// Run is unsupported off Windows.
func Run(name string, service Service, stopTimeout time.Duration) error {
	return ErrUnsupported
}
