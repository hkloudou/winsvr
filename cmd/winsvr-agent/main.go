// Command winsvr-agent is the service half of the pair: a self-updating Windows
// service that runs your payload ("helper.bin") inside the logged-on user's
// desktop session. It holds no business logic — your program lives in the
// payload (see cmd/example-helper).
//
// This file is a template. After `go get`, copy it, edit the constants below,
// build, and ship the two binaries. There are no command-line flags on purpose:
// configuration is code you own.
//
//	winsvr-agent install     # register + start the service (self-elevates via UAC)
//	winsvr-agent uninstall
//	winsvr-agent status
//	winsvr-agent run         # foreground (debug); Ctrl+C to stop
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
	Arguments:    []string{"run"},
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
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = strings.ToLower(os.Args[1])
	}
	if err := dispatch(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dispatch(cmd string) error {
	switch cmd {
	case "install":
		if err := selfElevate(); err != nil {
			return err
		}
		if err := winsvr.Install(config); err != nil {
			return err
		}
		fmt.Println("installed:", config.Name)
		if err := winsvr.Start(config.Name); err != nil {
			fmt.Fprintln(os.Stderr, "warning: not started:", err)
		}
		return nil
	case "uninstall", "remove":
		if err := selfElevate(); err != nil {
			return err
		}
		if err := winsvr.Uninstall(config.Name); err != nil {
			return err
		}
		fmt.Println("uninstalled:", config.Name)
		return nil
	case "status":
		st, err := winsvr.Status(config.Name)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", config.Name, st)
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
		return fmt.Errorf("unknown command %q (install|uninstall|status|run)", cmd)
	}
}

// runLogger returns the logger for the "run" command: the Windows Event Log when
// running as a service (a service has no console), and verbose stderr logging
// when started interactively — so `winsvr-agent run` in a terminal shows the
// full trace for debugging.
func runLogger() (*slog.Logger, func() error) {
	if isSvc, _ := winsvr.IsWindowsService(); isSvc {
		if l, closeLog, err := winsvr.NewEventLogger(config.Name); err == nil {
			return l, closeLog
		}
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), nil
}

// selfElevate re-runs the current command as administrator if it is not already
// (install/uninstall need it), then exits the non-elevated process.
func selfElevate() error {
	if winsvr.IsElevated() {
		return nil
	}
	code, err := winsvr.Elevate(context.Background(), os.Args[1:])
	if err != nil {
		return err
	}
	os.Exit(int(code))
	return nil
}
