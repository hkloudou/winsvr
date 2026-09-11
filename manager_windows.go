//go:build windows

package winsvr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func (c *Config) mgrStartType() uint32 {
	switch c.StartType {
	case StartManual:
		return mgr.StartManual
	case StartDisabled:
		return mgr.StartDisabled
	default:
		return mgr.StartAutomatic
	}
}

// Install registers the service and an event-log source. Requires admin.
func Install(c Config) error {
	if c.Name == "" {
		return errors.New("winsvr: Config.Name is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.CreateService(c.Name, exe, mgr.Config{
		DisplayName:      c.displayName(),
		Description:      c.Description,
		StartType:        c.mgrStartType(),
		Dependencies:     c.Dependencies,
		ServiceStartName: c.Account,
		Password:         c.Password,
	}, c.Arguments...)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_EXISTS) {
			return fmt.Errorf("service %q is already installed", c.Name)
		}
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	if c.RestartDelay > 0 {
		ra := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: c.RestartDelay}
		if err := s.SetRecoveryActions([]mgr.RecoveryAction{ra, ra, ra}, 86400); err != nil {
			return fmt.Errorf("set recovery actions: %w", err)
		}
	}
	if err := eventlog.InstallAsEventCreate(c.Name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!errors.Is(err, os.ErrExist) {
		return fmt.Errorf("install event source: %w", err)
	}
	return nil
}

// Uninstall stops and removes the service and its event-log source. Requires admin.
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("service %q is not installed", name)
		}
		return err
	}
	defer s.Close()
	// Stop the service and wait for it to actually reach Stopped before
	// deleting. Stopping cancels the service's context, which makes the
	// Supervisor terminate the payload (its job object is closed), so by the
	// time this returns the old helper is gone and its file is unlocked — which
	// is what makes a reinstall replace the helper cleanly.
	if status, err := s.Control(svc.Stop); err == nil && status.State != svc.Stopped {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			st, qerr := s.Query()
			if qerr != nil || st.State == svc.Stopped {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := eventlog.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove event source: %w", err)
	}
	return nil
}

// Status reports the current state of the named service. It needs only
// read-only query access, so it works without administrator rights. A missing
// service surfaces as an error wrapping ERROR_SERVICE_DOES_NOT_EXIST.
func Status(name string) (ServiceState, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, fmt.Errorf("connect SCM: %w", err)
	}
	defer windows.CloseServiceHandle(scm)
	np, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, err := windows.OpenService(scm, np, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return 0, err
	}
	return ServiceState(st.CurrentState), nil
}

// Start starts an installed service.
func Start(name string, args ...string) error {
	return control(name, func(s *mgr.Service) error { return s.Start(args...) })
}

// Stop asks an installed service to stop.
func Stop(name string) error {
	return control(name, func(s *mgr.Service) error { _, err := s.Control(svc.Stop); return err })
}

func control(name string, fn func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

// IsElevated reports whether the current process is running as administrator.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// ErrElevationCancelled is returned by Elevate when the user dismisses UAC.
var ErrElevationCancelled = errors.New("winsvr: elevation cancelled by user")

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

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

// Elevate re-launches the current executable elevated (UAC prompt) with args and
// waits for it, returning its exit code. If already elevated it returns (0, nil)
// without relaunching. Use it to self-elevate an install/uninstall command.
func Elevate(ctx context.Context, args []string) (uint32, error) {
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
	const seeMaskNoCloseProcess = 0x00000040
	info := shellExecuteInfo{Mask: seeMaskNoCloseProcess, Verb: verb, File: file, Parameters: params, Directory: dir, Show: windows.SW_SHOWNORMAL}
	info.Size = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(e, windows.ERROR_CANCELLED) {
			return 0, ErrElevationCancelled
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
