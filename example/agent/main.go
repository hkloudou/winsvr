// Command agent is the service half of the pair: a self-updating Windows
// service that runs your payload ("helper.bin") inside the logged-on user's
// desktop session. It holds no business logic — your program lives in the
// payload (see example/helper).
//
// It is an example/template. Copy it into your own module, edit the constants
// below, and build. There are no command-line flags on purpose: configuration
// is code you own.
//
//	agent            # double-click in Explorer: toggle install/uninstall, with a popup
//	agent install    # register + start the service (self-elevates via UAC)
//	agent uninstall
//	agent status
//	agent run        # foreground (debug); Ctrl+C to stop
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/hkloudou/winsvr"
)

// ─── edit these ───────────────────────────────────────────────────────────────
const (
	serviceName = "winsvr-agent"
	displayName = "winsvr Self-Updating Agent"
	description = "Keeps helper.bin up to date and runs it in the active user session."

	helperBin = "helper.bin" // payload file name, next to this executable
	updateURL = ""           // e.g. "https://dl.example.com/helper.bin"; "" disables auto-update
	sidecar   = false        // true: check <updateURL>.json (version+crc64) instead of HEAD/ETag
)

var config = winsvr.Config{
	Name:         serviceName,
	DisplayName:  displayName,
	Description:  description,
	StartType:    winsvr.StartAutomatic,
	Account:      `NT AUTHORITY\LocalSystem`, // required to launch into a user session
	RestartDelay: 5 * time.Second,
	Arguments:    []string{"run"}, // the SCM launches the service as `agent run`
}

func supervisor() *winsvr.Supervisor {
	return &winsvr.Supervisor{
		Bin:       helperBin,
		UpdateURL: updateURL,
		Sidecar:   sidecar,
		Hidden:    true, // no console window for the payload
	}
}

// ──────────────────────────────────────────────────────────────────────────────

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = strings.ToLower(os.Args[1])
	}
	if cmd == "" {
		// No subcommand. The SCM launches the service as `agent run`, so it never
		// lands here; this is a human running the exe directly. A double-click in
		// Explorer and a bare shell invocation both mean "manage me".
		if isSvc, _ := winsvr.IsWindowsService(); isSvc {
			cmd = "run"
		} else {
			cmd = "manage"
		}
	}
	if err := dispatch(cmd); err != nil {
		report("错误 / error: " + err.Error())
		os.Exit(1)
	}
}

func dispatch(cmd string) error {
	switch cmd {
	case "manage":
		// Toggle: install if the service is absent, uninstall if present. Status
		// is a read-only query (no admin needed) so the decision is made before
		// elevating.
		if _, err := winsvr.Status(config.Name); err == nil {
			return dispatch("uninstall")
		}
		return dispatch("install")

	case "install":
		if err := ensureAdmin("install"); err != nil {
			return err
		}
		if err := winsvr.Install(config); err != nil {
			return err
		}
		if err := winsvr.Start(config.Name); err != nil {
			// The service is registered but not running, so do not claim it
			// started. A wrong account or a bad path shows up here.
			return fmt.Errorf("%s installed but did not start: %w", config.Name, err)
		}
		report("服务已安装并启动 / service installed and started: " + config.Name)
		return nil

	case "uninstall", "remove":
		if err := ensureAdmin("uninstall"); err != nil {
			return err
		}
		if err := winsvr.Uninstall(config.Name); err != nil {
			return err
		}
		report("服务已卸载 / service uninstalled: " + config.Name)
		return nil

	case "status":
		st, err := winsvr.Status(config.Name)
		if err != nil {
			return err
		}
		report(fmt.Sprintf("%s: %s", config.Name, st))
		return nil

	case "run":
		logger, closeLog := runLogger()
		if closeLog != nil {
			defer closeLog()
		}
		sup := supervisor()
		sup.Logger = logger
		return winsvr.Run(config.Name, sup, config.StopTimeout)

	default:
		return fmt.Errorf("unknown command %q (manage|install|uninstall|status|run)", cmd)
	}
}

// report tells the user what happened: a message box for a double-click launch
// (where a console would vanish when the process exits) and stdout otherwise.
func report(msg string) {
	if winsvr.LaunchedByDoubleClick() {
		_ = winsvr.MessageBox(displayName, msg)
	} else {
		fmt.Println(msg)
	}
}

// ensureAdmin relaunches the current exe elevated to run cmd (install/uninstall
// need administrator rights), then exits the non-elevated process. It is a
// no-op when already elevated.
func ensureAdmin(cmd string) error {
	if winsvr.IsElevated() {
		return nil
	}
	code, err := winsvr.Elevate(context.Background(), []string{cmd})
	if err != nil {
		return err
	}
	os.Exit(int(code))
	return nil
}

// runLogger returns the logger for the "run" command: the Windows Event Log when
// running as a service (a service has no console), and verbose stderr logging
// when started interactively — so `agent run` in a terminal shows the full trace.
func runLogger() (*slog.Logger, func() error) {
	if isSvc, _ := winsvr.IsWindowsService(); isSvc {
		if l, closeLog, err := winsvr.NewEventLogger(config.Name); err == nil {
			return l, closeLog
		}
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), nil
}
