//go:build windows

package winsvr

import (
	"errors"

	"golang.org/x/sys/windows"
)

// ErrNoManifest is returned by ReadManifest when the running executable has no
// embedded RT_MANIFEST resource.
var ErrNoManifest = errors.New("winsvr: no embedded RT_MANIFEST resource")

// ReadManifest reads and parses the application manifest embedded in the
// currently running executable.
func ReadManifest() (*Manifest, error) {
	// A NULL module handle selects the image used to create the current
	// process; x/sys exposes no GetModuleHandle wrapper, and 0 is equivalent.
	const self windows.Handle = 0
	res, err := windows.FindResource(self, windows.CREATEPROCESS_MANIFEST_RESOURCE_ID, windows.RT_MANIFEST)
	if err != nil {
		if errors.Is(err, windows.ERROR_RESOURCE_TYPE_NOT_FOUND) || errors.Is(err, windows.ERROR_RESOURCE_NAME_NOT_FOUND) {
			return nil, ErrNoManifest
		}
		return nil, err
	}
	data, err := windows.LoadResourceData(self, res)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}
