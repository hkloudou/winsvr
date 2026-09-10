//go:build windows

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// State mirrors the WTS connection states of interest.
const (
	StateActive       = windows.WTSActive
	StateConnected    = windows.WTSConnected
	StateDisconnected = windows.WTSDisconnected
)

// NoSession is the sentinel session id returned when there is no active console
// session (e.g. at the logon screen, before any user signs in).
const NoSession uint32 = 0xFFFFFFFF

// Session describes a Windows session.
type Session struct {
	ID      uint32
	Station string
	State   uint32
}

// List enumerates all sessions on the local machine.
func List() ([]Session, error) {
	var p *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &p, &n); err != nil {
		return nil, fmt.Errorf("WTSEnumerateSessions: %w", err)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(p)))
	out := make([]Session, 0, n)
	for _, s := range unsafe.Slice(p, n) {
		out = append(out, Session{ID: s.SessionID, Station: windows.UTF16PtrToString(s.WindowStationName), State: s.State})
	}
	return out, nil
}

// ActiveConsole returns the session id currently attached to the physical
// console, and whether a user is signed in there. It first looks for a WTS
// session in the Active state, then falls back to WTSGetActiveConsoleSessionId.
func ActiveConsole() (id uint32, ok bool) {
	if sessions, err := List(); err == nil {
		for _, s := range sessions {
			if s.State == StateActive {
				return s.ID, true
			}
		}
	}
	id = windows.WTSGetActiveConsoleSessionId()
	return id, id != NoSession
}

// WaitForActiveConsole blocks until a user is signed in to the console session
// or ctx is cancelled, polling at the given interval.
func WaitForActiveConsole(ctx context.Context, poll time.Duration) (uint32, error) {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		if id, ok := ActiveConsole(); ok {
			return id, nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return NoSession, ctx.Err()
		case <-t.C:
		}
	}
}

// userToken returns a primary token for the user of sessionID. It requires
// SeTcbPrivilege, i.e. the caller must run as LocalSystem. When elevated is
// true and the user's default token is a UAC-filtered standard token, the
// linked full-admin token is used so the launched process is elevated without a
// UAC prompt (only possible from LocalSystem).
func userToken(sessionID uint32, elevated bool) (windows.Token, error) {
	var user windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &user); err != nil {
		return 0, fmt.Errorf("WTSQueryUserToken(session %d): %w", sessionID, err)
	}
	defer user.Close()

	src := user
	if elevated && !user.IsElevated() {
		linked, err := user.GetLinkedToken()
		if err != nil {
			return 0, fmt.Errorf("session %d has no elevated linked token (user is not an admin?): %w", sessionID, err)
		}
		defer linked.Close()
		src = linked
	}

	var primary windows.Token
	if err := windows.DuplicateTokenEx(src, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	return primary, nil
}

// LaunchOptions configures LaunchInSession.
type LaunchOptions struct {
	// Path is the full path to the executable to run. Required.
	Path string
	// Args are passed to the process (Path is prepended automatically).
	Args []string
	// Dir is the working directory. Defaults to the directory of Path.
	Dir string
	// Elevated runs the process with the user's linked admin token.
	//
	// IMPORTANT: if Path's manifest requests "requireAdministrator" it can only
	// be launched with an elevated token — with a standard token
	// CreateProcessAsUser fails with ERROR_ELEVATION_REQUIRED. A payload meant
	// to run as the plain logged-on user must be manifested "asInvoker".
	Elevated bool
	// Hidden creates the process with no window (CREATE_NO_WINDOW). Otherwise a
	// new console is allocated.
	Hidden bool
	// Desktop selects the window station\desktop. Defaults to `winsta0\default`.
	Desktop string
	// Job, if non-zero, is a job object the child is assigned to instead of a
	// freshly created kill-on-close one. Use NewKillOnCloseJob to share one job
	// across several launches.
	Job windows.Handle
}

// Process is a handle to a launched process (and, when it owns one, the
// kill-on-close job wrapping its process tree).
type Process struct {
	PID    uint32
	handle windows.Handle
	job    windows.Handle // non-zero only when owned by this Process
}

// NewKillOnCloseJob creates a job object whose assigned processes are all
// terminated when the last handle to the job is closed. Assign a service's
// child processes to it so they cannot outlive the service.
func NewKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return job, nil
}

// LaunchInSession starts o.Path inside sessionID's desktop as that session's
// user. The child is created suspended, assigned to a kill-on-close job, then
// resumed — so a crash of the parent service reliably takes the child (and its
// descendants) down with it.
func LaunchInSession(sessionID uint32, o LaunchOptions) (*Process, error) {
	if o.Path == "" {
		return nil, errors.New("session: LaunchOptions.Path is required")
	}
	tok, err := userToken(sessionID, o.Elevated)
	if err != nil {
		return nil, err
	}
	defer tok.Close()

	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, tok, false); err != nil {
		return nil, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(env)

	job, ownJob := o.Job, false
	if job == 0 {
		if job, err = NewKillOnCloseJob(); err != nil {
			return nil, err
		}
		ownJob = true
	}
	fail := func(e error) (*Process, error) {
		if ownJob {
			windows.CloseHandle(job)
		}
		return nil, e
	}

	desktop := o.Desktop
	if desktop == "" {
		desktop = `winsta0\default`
	}
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	si.Desktop, _ = windows.UTF16PtrFromString(desktop)

	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if o.Hidden {
		flags |= windows.CREATE_NO_WINDOW
	} else {
		flags |= windows.CREATE_NEW_CONSOLE
		si.Flags |= windows.STARTF_USESHOWWINDOW
		si.ShowWindow = windows.SW_SHOWNORMAL
	}

	dir := o.Dir
	if dir == "" {
		dir = filepath.Dir(o.Path)
	}
	exe16, _ := windows.UTF16PtrFromString(o.Path)
	cmd16, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{o.Path}, o.Args...)))
	dir16, _ := windows.UTF16PtrFromString(dir)

	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(tok, exe16, cmd16, nil, nil, false, flags, env, dir16, &si, &pi); err != nil {
		return fail(fmt.Errorf("CreateProcessAsUser(%s): %w", o.Path, err))
	}
	defer windows.CloseHandle(pi.Thread)

	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return fail(fmt.Errorf("AssignProcessToJobObject: %w", err))
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return fail(fmt.Errorf("ResumeThread: %w", err))
	}

	p := &Process{PID: pi.ProcessId, handle: pi.Process}
	if ownJob {
		p.job = job
	}
	return p, nil
}

const waitTimeout = 258 // WAIT_TIMEOUT

// Wait blocks until the process exits (returning its exit code) or ctx is
// cancelled.
func (p *Process) Wait(ctx context.Context) (uint32, error) {
	for {
		ev, err := windows.WaitForSingleObject(p.handle, 250)
		if err != nil {
			return 0, err
		}
		switch ev {
		case windows.WAIT_OBJECT_0:
			var code uint32
			if err := windows.GetExitCodeProcess(p.handle, &code); err != nil {
				return 0, err
			}
			return code, nil
		case waitTimeout:
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}
		default:
			return 0, fmt.Errorf("WaitForSingleObject: unexpected result %#x", ev)
		}
	}
}

// Kill terminates the process.
func (p *Process) Kill() error { return windows.TerminateProcess(p.handle, 1) }

// Close releases the process handle. If this Process owns its job object,
// closing it terminates the whole process tree.
func (p *Process) Close() error {
	windows.CloseHandle(p.handle)
	if p.job != 0 {
		j := p.job
		p.job = 0
		return windows.CloseHandle(j)
	}
	return nil
}

// FindByPath returns the PIDs of running processes whose full image path equals
// path (case-insensitive). Matching on the full path — not just the base name
// as naive watchdogs do — avoids killing unrelated processes that happen to
// share a file name.
func FindByPath(path string) ([]uint32, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	base := strings.ToLower(filepath.Base(abs))
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var pids []uint32
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.ToLower(windows.UTF16ToString(pe.ExeFile[:])) != base {
			continue
		}
		h, oerr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if oerr != nil {
			continue
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil &&
			strings.EqualFold(windows.UTF16ToString(buf[:n]), abs) {
			pids = append(pids, pe.ProcessID)
		}
		windows.CloseHandle(h)
	}
	return pids, nil
}

// KillByPath terminates every process whose full image path equals path.
func KillByPath(path string) (killed int, err error) {
	pids, err := FindByPath(path)
	if err != nil {
		return 0, err
	}
	self := uint32(os.Getpid())
	for _, pid := range pids {
		if pid == self {
			continue
		}
		h, oerr := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
		if oerr != nil {
			continue
		}
		if windows.TerminateProcess(h, 1) == nil {
			killed++
		}
		windows.CloseHandle(h)
	}
	return killed, nil
}

// Launcher owns a single kill-on-close job object and assigns every process it
// launches to it, so closing the Launcher terminates all of them and their
// descendants. A service should hold one Launcher for its lifetime.
type Launcher struct {
	job windows.Handle
}

// NewLauncher creates a Launcher backed by a fresh kill-on-close job.
func NewLauncher() (*Launcher, error) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		return nil, err
	}
	return &Launcher{job: job}, nil
}

// Launch starts o.Path in sessionID's desktop, assigned to the Launcher's
// shared job. The returned Process does not own the job; Launcher.Close does.
func (l *Launcher) Launch(sessionID uint32, o LaunchOptions) (*Process, error) {
	o.Job = l.job
	return LaunchInSession(sessionID, o)
}

// Close terminates every process assigned to the Launcher's job and releases
// the job handle.
func (l *Launcher) Close() error {
	if l.job == 0 {
		return nil
	}
	j := l.job
	l.job = 0
	return windows.CloseHandle(j)
}
