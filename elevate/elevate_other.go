//go:build !windows

package elevate

import (
	"context"
	"errors"
)

var ErrCancelled = errors.New("elevate: elevation was cancelled by the user")

func IsElevated() bool { return false }
func Relaunch(ctx context.Context, args []string) (uint32, error) {
	return 0, errors.New("elevate: only supported on windows")
}
