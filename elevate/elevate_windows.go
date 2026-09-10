//go:build windows

// Package elevate relaunches the current program elevated through the UAC
// prompt. Useful so an install/uninstall command can self-elevate instead of
// requiring the user to right-click "Run as administrator".
package elevate

import (
	"context"
	"errors"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrCancelled is returned when the user dismisses the UAC prompt.
var ErrCancelled = errors.New("elevate: elevation was cancelled by the user")

// IsElevated reports whether the current process is running elevated.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

var (
	modshell32          = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = modshell32.NewProc("ShellExecuteExW")
)

// shellExecuteInfo mirrors SHELLEXECUTEINFOW. x/sys only exposes ShellExecute,
// which returns no process handle to wait on, so we declare the struct here.
type shellExecuteInfo struct {
	Size       uint32
	Mask       uint32
	Hwnd       windows.Handle
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	InstApp    windows.Handle
	IDList     uintptr
	Class      *uint16
	KeyClass   windows.Handle
	HotKey     uint32
	Icon       windows.Handle
	Process    windows.Handle
}

const seeMaskNoCloseProcess = 0x00000040

// Relaunch restarts the current executable elevated, passing args, and waits
// for the elevated instance to exit, returning its exit code. If the current
// process is already elevated it returns (0, nil) without relaunching, so
// callers can guard with:
//
//	if !elevate.IsElevated() {
//		code, err := elevate.Relaunch(ctx, os.Args[1:])
//		...
//		os.Exit(int(code))
//	}
func Relaunch(ctx context.Context, args []string) (uint32, error) {
	if IsElevated() {
		return 0, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	escaped := make([]string, len(args))
	for i, a := range args {
		escaped[i] = windows.EscapeArg(a)
	}
	wd, _ := os.Getwd()

	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(escaped, " "))
	dir, _ := windows.UTF16PtrFromString(wd)

	info := shellExecuteInfo{
		Mask: seeMaskNoCloseProcess, Verb: verb, File: file,
		Parameters: params, Directory: dir, Show: windows.SW_SHOWNORMAL,
	}
	info.Size = uint32(unsafe.Sizeof(info))

	if r, _, e := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(e, windows.ERROR_CANCELLED) {
			return 0, ErrCancelled
		}
		return 0, e
	}
	if info.Process == 0 {
		return 0, nil
	}
	defer windows.CloseHandle(info.Process)

	for {
		ev, err := windows.WaitForSingleObject(info.Process, 250)
		if err != nil {
			return 0, err
		}
		if ev == windows.WAIT_OBJECT_0 {
			var code uint32
			windows.GetExitCodeProcess(info.Process, &code)
			return code, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
	}
}
