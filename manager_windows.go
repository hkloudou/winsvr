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
		DisplayName: c.displayName(),
		Description: c.Description,
		StartType:   c.mgrStartType(),
		// Left at its zero value this is SERVICE_ERROR_IGNORE, under which the
		// SCM records nothing when the service fails to start. Match what
		// sc.exe does, so a failed start is visible in the event log.
		ErrorControl:     mgr.ErrorNormal,
		Dependencies:     c.Dependencies,
		ServiceStartName: c.Account,
		Password:         c.Password,
	}, c.Arguments...)
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_SERVICE_EXISTS):
			return fmt.Errorf("service %q is already installed", c.Name)
		case errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE):
			// A previous uninstall deleted the service while it was still
			// running, so the registration lingers until that process exits.
			// Without this case the caller sees a bare error code.
			return fmt.Errorf("service %q is still being removed; wait for its old process to exit, or reboot, then install again", c.Name)
		}
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// The service exists from here on. A later failure would otherwise leave it
	// half-installed, and the retry that follows would be refused as "already
	// installed", so undo it instead. Only the service: the event source may
	// predate this install, which InstallAsEventCreate explicitly allows, and is
	// then not ours to remove.
	fail := func(err error) error {
		_ = s.Delete()
		return err
	}

	if c.RestartDelay > 0 {
		ra := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: c.RestartDelay}
		if err := s.SetRecoveryActions([]mgr.RecoveryAction{ra, ra, ra}, 86400); err != nil {
			return fail(fmt.Errorf("set recovery actions: %w", err))
		}
		// By default the SCM only runs recovery actions after a hard crash (the
		// process dies without reporting SERVICE_STOPPED). Our service reports a
		// clean STOPPED with a non-zero exit code when Run fails, so opt in to
		// recovery on those non-crash failures too — otherwise RestartDelay
		// would never fire for a normal error exit.
		if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
			return fail(fmt.Errorf("enable recovery on non-crash failures: %w", err))
		}
	}
	if err := eventlog.InstallAsEventCreate(c.Name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!errors.Is(err, os.ErrExist) {
		// This call writes its key before it can fail, so clear what it left.
		// Only here: on the paths above the source was never touched.
		_ = eventlog.Remove(c.Name)
		return fail(fmt.Errorf("install event source: %w", err))
	}
	return nil
}

// stopWait is how long Uninstall waits for a service to report Stopped before
// deleting it anyway.
const stopWait = 20 * time.Second

// Uninstall stops and removes the service and its event-log source. Requires
// admin. The removal happens even when the service cannot be stopped, whether it
// ran out of stopWait or refused the request outright; the returned error then
// says so, and that the removal completes when the service's process exits.
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
	// Stop first and wait for it to actually reach Stopped. Stopping cancels the
	// service's context, which makes the Supervisor terminate the payload (its
	// job object closes), so by the time this returns the old helper is gone and
	// its file unlocked — which is what lets a reinstall replace it cleanly.
	//
	// A stop that fails does not abort the removal, because removal is what this
	// function promises: dependent services, for one, make the stop impossible
	// while leaving the deletion perfectly possible. It is reported afterwards.
	stopped, stopErr := stopAndWait(s, name)
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := eventlog.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove event source: %w", err)
	}
	// Delete only marks a running service for removal; it stays registered, and
	// a reinstall stays refused, until its process exits. Saying so is what
	// keeps that from looking like an uninstall that did nothing.
	switch {
	case stopErr != nil:
		return fmt.Errorf("service %q was removed, but stopping it first failed, so it may still be running: %w", name, stopErr)
	case !stopped:
		return fmt.Errorf("service %q was removed but did not stop within %s; it disappears once its process exits, which a reboot guarantees", name, stopWait)
	}
	return nil
}

// stopAndWait asks the service to stop and waits up to stopWait for it to get
// there, asking again while it is in a state that cannot accept the request yet.
// It reports whether the service actually reached Stopped.
//
// Control returns the service's most recent status alongside some errors, so the
// status has to be read even when the call failed. A service still starting
// refuses the control with ERROR_SERVICE_CANNOT_ACCEPT_CTRL while reporting
// StartPending; reading that as "already stopped" would delete a service that is
// still running, and its payload with it, while reporting success. That is also
// why the request is repeated: a service that never accepted a stop will finish
// starting and then sit there Running, with nothing having asked it to stop.
func stopAndWait(s *mgr.Service, name string) (bool, error) {
	deadline := time.Now().Add(stopWait)
	for {
		status, cerr := s.Control(svc.Stop)
		switch {
		case errors.Is(cerr, windows.ERROR_SERVICE_NOT_ACTIVE):
			return true, nil
		case cerr == nil,
			errors.Is(cerr, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL),
			errors.Is(cerr, windows.ERROR_INVALID_SERVICE_CONTROL):
			// Accepted, or refused because the service is mid-transition. Either
			// way status carries what it last reported.
			if status.State == svc.Stopped {
				return true, nil
			}
		default:
			return false, fmt.Errorf("stop service %q: %w", name, cerr)
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
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
			if err := windows.GetExitCodeProcess(info.Process, &code); err != nil {
				return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
			}
			return code, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
	}
}
