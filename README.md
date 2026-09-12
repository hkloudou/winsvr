# winsvr

Run a Go program as a Windows service, and — when you need it — a ready-made
**self-updating agent** that keeps a payload up to date from a URL and runs it
in the logged-on user's desktop session.

- One package, no ceremony: `Install` / `Uninstall` / `Status` / `Run` over
  `golang.org/x/sys/windows/svc`.
- Launch a program **as the interactive user** from a `LocalSystem` service,
  wrapped in a kill-on-close Job Object so it can't outlive the service.
- **Self-updating**: one cheap `HEAD` check at boot (or a `version`+`crc64`
  sidecar), verified install, crash-restart.
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
mostly leave alone; `example/agent` is a ~90-line template you copy and edit.
Your code goes in the payload (`example/helper` shows where), because that is
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
          ├─ 1. wait for network, then update check: HEAD helper.bin → newer? download + swap
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

`example/agent` wires this to a small CLI (no flags — configuration is code
you own):

```sh
agent               # double-click in Explorer: toggles install/uninstall, with a popup
agent install       # register + start; self-elevates via UAC
agent status
agent uninstall
agent run           # foreground, Ctrl+C to stop — for debugging
```

### Double-click: install / uninstall with a popup

Double-clicking the exe in Explorer toggles the service — install + start if it
is not present, uninstall if it is — then shows a message box with the result.
It tells a double-click apart from a shell run (and from the SCM) by whether it
owns a brand-new console alone (`GetConsoleProcessList == 1`); on a double-click
a console would vanish when the process exits, so feedback goes to a dialog
instead of stdout. Install and uninstall self-elevate through UAC.

Uninstall stops the service and waits for it to actually stop before removing
it — and stopping the service terminates the helper (its job object closes), so
a **reinstall replaces a cleanly stopped helper**, never a locked, running one.

### Update policy: checked once, at startup

The payload is updated **only when the service starts** — once per boot for an
auto-start service, and again on every SCM restart — and always **before** the
payload is launched. The sequence guarantees the payload that runs is current:

1. **Wait until the check passes.** It is retried until it succeeds, so a
   machine that boots offline does not run a possibly-stale payload; it waits
   for connectivity first. A server that cannot identify its payload holds
   things up the same way, on purpose: see **the payload must be
   identifiable** below. Retries and errors are logged.
2. **Compare.** Ask the server what the current payload is, and download when it
   differs from the one on disk, when there is none, or when the payload no
   longer matches the checksum it is supposed to have. That last case is what
   catches a payload corrupted or replaced locally while the remote stayed put.
3. **Verify, then swap.** Check the size and the checksum before anything is
   installed. The swap removes the old file and renames the new one into place,
   retrying for a moment, so a scanner holding a transient lock cannot leave the
   machine with no payload at all. It runs while the payload is stopped, so the
   file is not in use. In ETag mode the validator is recorded from the `GET`
   rather than the `HEAD`: if a release lands between the two, the body is read
   again, so what gets stored always describes the bytes on disk.
4. Launch the now-current payload in the user's session.
5. Supervise it (restart on crash). **No further update checks** until the next
   service start.

Crash-restarts of the payload reuse the current binary; a fresh check happens
only the next time the service itself starts. Pick the strategy with one field:

```go
&winsvr.Supervisor{Bin: "helper.bin", UpdateURL: url}                 // HEAD: ETag, required
&winsvr.Supervisor{Bin: "helper.bin", UpdateURL: url, Sidecar: true}  // GET url+".json": {version, size, crc64}
```

The sidecar (`helper.bin.json`, e.g.
`{"version":"1.4.0","size":12345,"crc64":"a1b2…"}`) adds semantic versions and an
end-to-end CRC-64/ECMA integrity check.

#### The payload must be identifiable

Neither mode guesses. If the server cannot say which payload it is serving, the
check fails, it is retried, and **the payload does not launch** until the server
is fixed. The log says which of the two it is.

- **ETag mode requires an `ETag` header**, on the `HEAD` and on the `GET`. It is
  the only validator this mode has. Comparing `Content-Length` instead would
  compare a number a rebuild can leave unchanged, so a missing `ETag` is
  reported as the server misconfiguration it is rather than worked around.
  On nginx, `ETag` is on by default for static files; S3 and most CDNs send one.
- **Sidecar mode requires `crc64`.** It is both what the mode compares and what
  it verifies, so a sidecar without one identifies nothing. Reading it as
  "nothing to verify" would let a server switch checking off by dropping a
  field.

#### Where the state lives

ETag mode writes `helper.bin.update.json` next to the payload, holding the ETag
it last installed and that payload's checksum.

The ETag is the reason the file exists. HTTP defines it as an **opaque**
validator, so it cannot be assumed to follow from the payload's content. Some
servers do make it a content hash, and S3 uses the MD5 hex for a single-part
upload. Many do not: nginx builds it from the modification time and the length,
Apache from the modification time and the size, and an S3 multipart upload gives
a digest of the part digests rather than of the file. Redeploying identical bytes
changes the first two. So the value the server sent has to be stored, because it
cannot be recomputed.

If you control the server and can publish a content hash, that is what sidecar
mode is, and it needs no state file.

Sidecar mode keeps **no state at all**. The sidecar already publishes the
payload's checksum, so comparing that with the file on disk answers both
questions at once, whether a new release exists and whether the payload is still
the one that was installed. A leftover state file from ETag mode is deleted.

Two more fields matter for a large payload. `HTTPClient` replaces the default
client, which allows five minutes for one whole request, body included; supply
your own with no `Timeout` when a slow link needs longer and let the service's
context bound the transfer instead. `MaxUpdateBytes` moves the size cap.

### What auto-update does not protect you from

Read this before pointing `UpdateURL` at anything. The check defends against a
stale or corrupted payload. It does not defend against an attacker.

- **CRC-64 is not a signature.** It catches accidental corruption. It is not a
  cryptographic hash, and it is linear, so producing content that matches a
  given checksum is easy. The sidecar also travels over the same connection as
  the payload, so whoever can rewrite one can rewrite the other. For
  authenticity, verify a real signature over the downloaded file against a
  public key compiled into the service, before it is installed.
- **Use HTTPS.** Nothing here rejects an `http://` URL. The default client does
  refuse a redirect that drops TLS, so an `https://` URL cannot be steered onto
  plain HTTP, but that only helps if you started with `https://`.
- **The install directory is the real trust boundary.** The payload sits beside
  the service exe, and the service runs as `LocalSystem`. Any user who can write
  that directory can replace either binary, and replacing the service exe means
  SYSTEM at the next boot. Install under `%ProgramFiles%`, or anywhere else only
  administrators can write. Never install from a download folder.
- **`Elevated` is not a privilege boundary.** It launches the payload with the
  signed-in user's linked admin token, inside that user's own session, where
  they can debug it or inject into it. It also needs that user to be an
  administrator: a standard user has no linked token, and the launch fails.

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
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w"               -o dist/winsvr-agent.exe ./example/agent
# payload: no console window (-H windowsgui) so nothing flashes when it launches
CGO_ENABLED=0 GOOS=windows GOARCH=386  go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/helper.bin      ./example/helper
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
winsvr.go, service_*, manager_*, session_*, eventlog_*, interact_*   the winsvr package
update.go, supervisor.go                                             update + the recipe (pure Go)
example/agent/    the service — an example template you copy and edit (no business logic)
example/helper/   the payload — where YOUR program goes (runs as the user)
```

Everything compiles on all platforms; Windows-only calls return `ErrUnsupported`
off Windows, and the update logic is pure Go and unit-tested anywhere.
