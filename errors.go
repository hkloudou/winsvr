package winsvr

import "errors"

// ErrUnsupported is returned by Windows-only operations when they are called on
// a non-Windows platform.
var ErrUnsupported = errors.New("winsvr: operation is only supported on windows")
