// Package winsvr helps you ship a Go program as a Windows service with as
// little boilerplate as possible.
//
// It provides two layers:
//
//   - Core primitives — thin, idiomatic wrappers over
//     golang.org/x/sys/windows/svc for install/uninstall/status/run, plus the
//     session, token, process and job-object machinery needed to launch a
//     program inside the interactive desktop session of the logged-on user.
//     See package winsvr, and the subpackages session and elevate.
//
//   - A batteries-included Supervisor (see Supervise) that implements a common
//     real-world pattern: a service running as LocalSystem that keeps a payload
//     binary up to date from a remote URL (cheap HTTP HEAD checks) and runs it
//     in the logged-on user's session, restarting it if it crashes and killing
//     the whole tree when the service stops. See package update for the
//     update-check strategies.
//
// Everything compiles on every GOOS: non-Windows builds get stubs that return
// ErrUnsupported, so you can keep unit tests for your pure business logic
// running on Linux and macOS CI.
package winsvr
