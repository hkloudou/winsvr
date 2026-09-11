//go:build windows

package winsvr

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// LaunchedByDoubleClick reports whether the executable was started by
// double-clicking it in Explorer, as opposed to being run from a shell or by
// the Service Control Manager.
//
// The test: a program launched from Explorer gets a brand-new console that it
// alone is attached to, so GetConsoleProcessList returns 1; a program started
// from an existing cmd/PowerShell shares that shell's console, so the count is
// >= 2. Services have no console. Use it to decide whether to talk to the user
// through a dialog (double-click, where console output would vanish when the
// window closes) or through stdout (shell).
func LaunchedByDoubleClick() bool {
	if isSvc, _ := IsWindowsService(); isSvc {
		return false
	}
	var pids [2]uint32
	r, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return r == 1
}

// MessageBox shows a simple modal information dialog. It is meant for the
// double-click case, where writing to a console is pointless because the window
// closes when the process exits. (A tray balloon / toast is possible but needs
// a notification icon and a message loop; a message box is one call and always
// visible.)
func MessageBox(title, text string) error {
	t, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return err
	}
	c, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	_, err = windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONINFORMATION)
	return err
}
