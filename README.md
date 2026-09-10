# winsvr

Run a Go program as a Windows service, and — when you need it — a ready-made
**self-updating agent** that keeps a payload up to date from a URL and runs it
in the logged-on user's desktop session.

- One package, no ceremony: `Install` / `Uninstall` / `Status` / `Run` over
  `golang.org/x/sys/windows/svc`.
- Launch a program **as the interactive user** from a `LocalSystem` service,
  wrapped in a kill-on-close Job Object so it can't outlive the service.
- **Self-updating**: one cheap `HEAD` check at boot (or a `version`+`crc64`
  sidecar), atomic swap, crash-restart.
- Builds on every OS — non-Windows gets stubs, so your logic stays testable on
  Linux/macOS CI.

Targets **Windows 10 / Server 2016 and later**.

```sh
go get github.com/hkloudou/winsvr
```

## Two programs, one job each

A background agent that acts on behalf of a signed-in user is naturally **two
binaries**. Keep them straight and everything else follows:

| | **the service** — `winsvr-agent` | **the payload** — `helper.bin` |
|---|---|---|
| runs as | `LocalSystem` (session 0, no desktop) | the **logged-on user**, in their desktop |
| contains | **no business logic** | **your actual program** |
| its job | install itself, update the payload, launch it in the user session, restart it on crash, kill it on stop | whatever your app does: UI, tray icon, automation, talking to the user's session |
| lifetime | installed once, auto-starts at boot | started and supervised by the service |
| manifest | `requireAdministrator`, console subsystem | `asInvoker`, windowless (`-H windowsgui`) |

**Write your program in the payload.** The service is plumbing you configure and
mostly leave alone; `cmd/winsvr-agent` is a ~90-line template you copy and edit.
Your code goes in the payload (`cmd/example-helper` shows where), because that is
what runs *as the user*.

Why it has to be split:

- **A service can't be the user.** It runs in session 0 with no desktop, as
  `SYSTEM`. It cannot show a window, read the user's files as them, or use their
  credentials. Most real work needs to run *as the user* — that's the payload.
- **You can't replace a running .exe.** Auto-update needs one program (the
  service) to stop, swap, and relaunch another (the payload). Two binaries make
  that safe; a single self-replacing exe does not.

```
boot ─▶ SCM starts winsvr-agent (SYSTEM)
          │
          ├─ 1. update check (once): HEAD helper.bin → newer? download + swap
          ├─ 2. wait for a signed-in user, then CreateProcessAsUser(helper.bin)
          └─ 3. supervise: restart on crash · kill the tree on service stop
                             helper.bin runs here, as the user ◀── your code
```

## Using it

After `go get`, you write a tiny service `main` and your payload `main`. The
service is just a `Config` plus a `Supervisor`:

```go
var config = winsvr.Config{
    Name:         "winsvr-agent",
    DisplayName:  "winsvr Self-Updating Agent",
    StartType:    winsvr.StartAutomatic,
    Account:      `NT AUTHORITY\LocalSystem`, // required to enter a user session
    RestartDelay: 5 * time.Second,
    Arguments:    []string{"run"},
}

sup := &winsvr.Supervisor{
    Bin:       "helper.bin",                       // resolved next to the service .exe
    UpdateURL: "https://dl.example.com/helper.bin", // "" disables auto-update
    Hidden:    true,
}

winsvr.Run(config.Name, sup, config.StopTimeout)   // SCM when a service; console otherwise
```

`cmd/winsvr-agent` wires this to a small CLI (no flags — configuration is code
you own):

```sh
winsvr-agent install     # register + start; self-elevates via UAC
winsvr-agent status
winsvr-agent uninstall
winsvr-agent run         # foreground, Ctrl+C to stop — for debugging
```

### Update policy: checked once, at startup

The payload is updated **only when the service starts** (i.e. once per boot for
an auto-start service). The sequence guarantees the payload that runs is current:

1. `HEAD` the URL; if the payload is newer (or missing), `GET`, verify, atomic
   swap — done while the payload is stopped, so replacing the file is safe.
2. Launch the now-current payload in the user's session.
3. Supervise it (restart on crash). **No further update checks** until the next
   service start.

An offline check does not block startup — the existing payload runs. Pick the
strategy with one field:

```go
&winsvr.Supervisor{Bin: "helper.bin", UpdateURL: url}                 // HEAD: ETag / Content-Length
&winsvr.Supervisor{Bin: "helper.bin", UpdateURL: url, Sidecar: true}  // GET url+".json": {version, crc64}
```

The sidecar (`helper.bin.json`, e.g. `{"version":"1.4.0","crc64":"a1b2…"}`) adds
semantic versions and an end-to-end CRC-64/ECMA integrity check.

### Using the primitives directly

The `Supervisor` is optional. The building blocks are exported:

```go
winsvr.Install(cfg) / winsvr.Uninstall(name) / winsvr.Status(name)
winsvr.Run(name, svc, stopTimeout)                 // implement winsvr.Service: Run(ctx) error
sid, ok := winsvr.ActiveConsoleSession()
p, _ := winsvr.LaunchInSession(sid, winsvr.LaunchOptions{Path: bin, Hidden: true})
if !winsvr.IsElevated() { winsvr.Elevate(ctx, os.Args[1:]) }
log, closeLog, _ := winsvr.NewEventLogger(cfg.Name)  // a service has no stdout
```

## Building the two binaries

No C toolchain needed: the default cross-compile uses `CGO_ENABLED=0` and Go's
internal linker.

```sh
make tools     # go install github.com/tc-hib/go-winres@latest
make res       # icon + manifest + version → rsrc_windows_{amd64,386}.syso
make agent     # dist/winsvr-agent.exe  (console, requireAdministrator)
make helper    # dist/helper.bin        (windowless, asInvoker)
```

Behind `make`:

```sh
# service: console subsystem so install/uninstall can print
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w"               -o dist/winsvr-agent.exe ./cmd/winsvr-agent
# payload: no console window (-H windowsgui) so nothing flashes when it launches
CGO_ENABLED=0 GOOS=windows GOARCH=386  go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/helper.bin      ./cmd/example-helper
```

**Building with cgo from macOS?** Install mingw-w64 and pass `CGO=1`
(`make agent CGO=1`), which sets `CC=x86_64-w64-mingw32-gcc` (or
`i686-w64-mingw32-gcc` for 386). `-H windowsgui` (the PE subsystem) and the
manifest (an RT_MANIFEST resource) are independent — the payload wants both.

> **Don't swap the manifests.** A payload launched as a standard user must be
> `asInvoker`; a `requireAdministrator` payload can't start via
> `CreateProcessAsUser` (`ERROR_ELEVATION_REQUIRED`). To run the payload
> elevated, set `Supervisor.Elevated` (the service holds the user's linked admin
> token) — don't change its manifest.

## Layout

```
winsvr.go, service_*, manager_*, session_*, eventlog_*   the winsvr package
update.go, supervisor.go                                 update + the recipe (pure Go)
cmd/winsvr-agent/    the service — a template you copy and edit (no business logic)
cmd/example-helper/  the payload — where YOUR program goes (runs as the user)
```

Everything compiles on all platforms; Windows-only calls return `ErrUnsupported`
off Windows, and the update logic is pure Go and unit-tested anywhere.
