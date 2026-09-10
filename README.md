# winsvr

Ship a Go program as a Windows service with almost no boilerplate — and, when
you need it, a batteries-included **self-updating agent** that keeps a payload
binary current from a URL and runs it inside the logged-on user's desktop
session.

- **Thin, typed core.** Install / uninstall / status / run on top of
  `golang.org/x/sys/windows/svc`. No hand-rolled `syscall.LazyProc`.
- **Run in the user's session.** Launch a process as the interactive user from a
  `LocalSystem` service, in that user's desktop, optionally elevated — wrapped
  in a kill-on-close **Job Object** so it can never outlive the service.
- **Self-updating supervisor.** Cheap HTTP `HEAD` update checks (ETag /
  `Content-Length`, or a `version` + `crc64` sidecar), atomic swap, crash
  restart with backoff.
- **Builds everywhere.** Non-Windows builds compile against stubs, so your
  business-logic tests keep running on Linux/macOS CI.

Targets **Windows 10 / Server 2016 and later**.

## Install

```sh
go get github.com/hkloudou/winsvr
```

## Two-binary model

A background agent that runs code in the user's session is naturally two
programs, and this repo ships both as a working template:

| Binary | Runs as | Subsystem | Manifest | Role |
| --- | --- | --- | --- | --- |
| `winsvr-agent` (`cmd/winsvr-agent`) | `LocalSystem` (service) | console | `requireAdministrator` | installs itself, updates the payload, launches it in the user session |
| `helper.bin` (`cmd/example-helper`) | the logged-on user | GUI (no console) | `asInvoker` | your actual workload |

> **The manifests are not interchangeable.** A payload launched as a standard
> user must be `asInvoker`; if it is `requireAdministrator`,
> `CreateProcessAsUser` fails with `ERROR_ELEVATION_REQUIRED`. Elevate the
> payload by setting `Policy.Elevated` (the service holds the user's linked
> admin token) — not by changing its manifest.

## Quickstart

The whole agent is a `Supervisor` plus a small CLI. Bin name and remote URL are
user choices, exposed as flags:

```go
cfg := winsvr.Config{
    Name:         "winsvr-agent",
    DisplayName:  "winsvr Self-Updating Agent",
    StartType:    winsvr.StartAutomatic,
    Account:      `NT AUTHORITY\LocalSystem`, // required to enter a user session
    RestartDelay: 5 * time.Second,
    Arguments:    []string{"run"},
}

sup := &winsvr.Supervisor{
    Bin:    "helper.bin",                       // --bin
    Update: &update.Source{URL: "https://dl.example.com/helper.bin"}, // --url
    Policy: winsvr.RunPolicy{Hidden: true, UpdateInterval: 30 * time.Minute},
}

winsvr.Run(cfg.Name, sup, cfg.StopTimeout)      // SCM when a service, console otherwise
```

Manage it (the agent self-elevates for install/uninstall):

```sh
winsvr-agent install     # register + start; prompts for UAC
winsvr-agent status
winsvr-agent uninstall
winsvr-agent run         # foreground, Ctrl+C to stop — for debugging
```

### What the supervisor does each cycle

1. **Update between runs** (payload stopped, so replacing the file is safe):
   `HEAD` the URL, compare the stored validator; if newer or the file is
   missing, `GET`, verify, atomic-swap. A failed check does **not** block
   startup — the existing payload runs (set `RunPolicy.RequireUpdate` to change
   that).
2. **Wait for an active console session**, then launch the payload as that user.
3. **Watch it.** On exit, restart with exponential backoff. With
   `UpdateInterval > 0`, poll for a newer payload while it runs and recycle it
   when one lands. When the service stops, the Job Object kills the payload and
   its whole process tree.

### Update strategies

```go
// Default: standard headers, works with any static host (nginx, S3, CDN).
&update.Source{URL: url}

// Sidecar: publish url+".json" as {"version","size","crc64"} for semantic
// versioning and end-to-end CRC-64/ECMA integrity.
&update.Source{URL: url, Validator: &update.SidecarValidator{BinaryURL: url}}
```

## Core API (use the pieces directly)

```go
// Service lifecycle
winsvr.Install(cfg) / winsvr.Uninstall(name) / winsvr.Status(name)
winsvr.Run(name, svc, stopTimeout)            // implement winsvr.Service: Run(ctx) error

// Launch into the user's session (package session)
l, _ := session.NewLauncher()                 // shared kill-on-close job
defer l.Close()
sid, _ := session.ActiveConsole()
proc, _ := l.Launch(sid, session.LaunchOptions{Path: bin, Hidden: true})

// Self-elevate (package elevate)
if !elevate.IsElevated() { elevate.Relaunch(ctx, os.Args[1:]) }

// Read your own manifest, log to the Windows Event Log
m, _ := winsvr.ReadManifest()
log, closeLog, _ := winsvr.NewEventLogger(cfg.Name)
```

## Building the binaries

No C toolchain is needed: the default cross-compile uses `CGO_ENABLED=0` and
Go's internal linker.

```sh
make tools           # go install github.com/tc-hib/go-winres@latest
make res             # icon + manifest + version -> rsrc_windows_{amd64,386}.syso
make agent helper    # -> dist/winsvr-agent.exe, dist/helper.bin
```

Behind `make`:

```sh
# service: console subsystem, requireAdministrator manifest
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/winsvr-agent.exe ./cmd/winsvr-agent
# payload: no console window (-H windowsgui), asInvoker manifest
CGO_ENABLED=0 GOOS=windows GOARCH=386  go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/helper.bin ./cmd/example-helper
```

**Using cgo from macOS?** Install mingw-w64 and build with `CGO=1`
(`make agent CGO=1`), which sets `CC=x86_64-w64-mingw32-gcc` (or
`i686-w64-mingw32-gcc` for 386). `-H windowsgui` works with the external linker
too. The `-H windowsgui` flag (PE subsystem) and the manifest (an RT_MANIFEST
resource) are independent — you want both on the payload.

## Platform support

Everything compiles on all platforms; Windows-only calls return `ErrUnsupported`
off Windows so you can unit-test your logic on any CI. The `update` package is
pure Go and fully testable everywhere.
