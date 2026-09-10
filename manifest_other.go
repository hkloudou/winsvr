//go:build !windows

package winsvr

import "errors"

// ErrNoManifest is returned when no RT_MANIFEST is embedded.
var ErrNoManifest = errors.New("winsvr: no embedded RT_MANIFEST resource")

// ReadManifest is unsupported off Windows.
func ReadManifest() (*Manifest, error) { return nil, ErrUnsupported }
