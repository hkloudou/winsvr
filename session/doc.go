// Package session launches processes inside the interactive desktop session of
// a logged-on user from a service running as LocalSystem, and enumerates the
// sessions available to launch into.
//
// This replaces hand-rolled syscall.LazyProc code: every primitive here is a
// typed wrapper already provided by golang.org/x/sys/windows.
package session
