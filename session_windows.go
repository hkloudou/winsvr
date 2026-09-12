//go:build windows

package winsvr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// noSession is the sentinel returned when no user is signed in to the console.
const noSession uint32 = 0xFFFFFFFF

// hasSignedInUser reports whether a user token can be obtained for a session.
//
// Only a caller holding SeTcbPrivilege, in practice LocalSystem, can ask at all.
// One without it is told so through ERROR_PRIVILEGE_NOT_HELD and gets true, so
// the session's own state still decides for it. Every other failure means nobody
// is signed in: the exact code a signed-out session reports is not consistent
// across Windows versions, and a service must not depend on telling them apart.
func hasSignedInUser(sid uint32) bool {
	var tok windows.Token
	err := windows.WTSQueryUserToken(sid, &tok)
	if err == nil {
		tok.Close()
		return true
	}
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

// ActiveConsoleSession returns the session id of a signed-in user, preferring the
// one attached to the physical console.
//
// A session is active with no user signed in whenever the machine sits at a logon
// screen, and launching into one can only fail. So a session counts here only
// once a user token exists for it; reporting it ready earlier is what made a
// service retry, and log, for as long as nobody signed in.
func ActiveConsoleSession() (uint32, bool) {
	console := windows.WTSGetActiveConsoleSessionId()
	if console != noSession && hasSignedInUser(console) {
		return console, true
	}
	var p *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &p, &n); err == nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(p)))
		for _, s := range unsafe.Slice(p, n) {
			if s.State == windows.WTSActive && s.SessionID != console && hasSignedInUser(s.SessionID) {
				return s.SessionID, true
			}
		}
	}
	return noSession, false
}

// WaitForActiveConsole blocks until a user is signed in to the console session
// or ctx is cancelled. It polls, and logs nothing, so a machine left at a logon
// screen simply waits.
func WaitForActiveConsole(ctx context.Context, poll time.Duration) (uint32, error) {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		if id, ok := ActiveConsoleSession(); ok {
			return id, nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return noSession, ctx.Err()
		case <-t.C:
		}
	}
}

// LaunchOptions configures a launch into a user session.
type LaunchOptions struct {
	Path     string   // executable path (required)
	Args     []string // arguments
	Dir      string   // working directory (defaults to Path's directory)
	Elevated bool     // launch with the user's linked admin token (needs the user to be an admin)
	Hidden   bool     // no window (CREATE_NO_WINDOW)
}

// Process is a launched payload process wrapped in a kill-on-close job.
type Process struct {
	PID    uint32
	handle windows.Handle
	job    windows.Handle
}

func userToken(sessionID uint32, elevated bool) (windows.Token, error) {
	var user windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &user); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			// Not running as LocalSystem. That is a configuration mistake, not a
			// session that is waiting for someone to sign in, so report it loudly.
			return 0, fmt.Errorf("WTSQueryUserToken(session %d): the service must run as LocalSystem: %w", sessionID, err)
		}
		// Anything else means the session has no signed-in user right now, most
		// often because they signed out between the session check and this call.
		return 0, fmt.Errorf("%w: session %d: %w", ErrNoUserSession, sessionID, err)
	}
	defer user.Close()
	src := user
	if elevated && !user.IsElevated() {
		linked, err := user.GetLinkedToken()
		if err != nil {
			return 0, fmt.Errorf("session %d has no elevated token: %w", sessionID, err)
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

// LaunchInSession starts o.Path in sessionID's desktop as that session's user.
// The process is wrapped in a kill-on-close job so it dies when the returned
// Process is Closed (or when the service process exits). The caller must run as
// LocalSystem.
func LaunchInSession(sessionID uint32, o LaunchOptions) (*Process, error) {
	if o.Path == "" {
		return nil, errors.New("winsvr: LaunchOptions.Path is required")
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

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	var jinfo windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	jinfo.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&jinfo)), uint32(unsafe.Sizeof(jinfo))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	si.Desktop, _ = windows.UTF16PtrFromString(`winsta0\default`)
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
		windows.CloseHandle(job)
		return nil, fmt.Errorf("CreateProcessAsUser(%s): %w", o.Path, err)
	}
	defer windows.CloseHandle(pi.Thread)
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	return &Process{PID: pi.ProcessId, handle: pi.Process, job: job}, nil
}

const waitTimeout = 258 // WAIT_TIMEOUT

// Wait blocks until the process exits (returning its exit code) or ctx is done.
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
				// Without this the zero value reads as a clean exit.
				return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
			}
			return code, nil
		case waitTimeout:
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}
		default:
			return 0, fmt.Errorf("WaitForSingleObject: %#x", ev)
		}
	}
}

// Close terminates the process (and its whole tree, via the job) and releases
// the handles. It is safe to call more than once: both handles are cleared, so a
// second call cannot close a descriptor Windows has since handed to something
// else.
func (p *Process) Close() error {
	if h := p.handle; h != 0 {
		p.handle = 0
		windows.CloseHandle(h)
	}
	if j := p.job; j != 0 {
		p.job = 0
		return windows.CloseHandle(j)
	}
	return nil
}
