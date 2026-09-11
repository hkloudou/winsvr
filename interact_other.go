//go:build !windows

package winsvr

// LaunchedByDoubleClick is always false off Windows.
func LaunchedByDoubleClick() bool { return false }

// MessageBox is unsupported off Windows.
func MessageBox(title, text string) error { return ErrUnsupported }
