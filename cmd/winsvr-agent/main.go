// Command winsvr-agent is a self-updating Windows service that runs a payload
// binary ("helper.bin" by default) inside the logged-on user's desktop session
// and keeps it up to date from a remote URL.
//
// It is both a working program and a template: copy it, change the defaults or
// wire the flags into your deployment, and ship two binaries — this agent
// (installed as a service, runs as LocalSystem) and your payload (built with
// `-ldflags -H windowsgui`, manifested "asInvoker").
//
//	winsvr-agent install     # register + start the service (self-elevates)
//	winsvr-agent uninstall   # stop + remove the service
//	winsvr-agent status
//	winsvr-agent run         # run in the foreground (debug; Ctrl+C to stop)
//
// Build resources (icon + requireAdministrator manifest) with `make res` before
// `go build`; see the Makefile.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hkloudou/winsvr"
	"github.com/hkloudou/winsvr/elevate"
	"github.com/hkloudou/winsvr/update"
)

// Defaults — override with flags at install time. These are the "bin name" and
// "remote URL" choices exposed to the user.
const (
	defaultName    = "winsvr-agent"
	defaultDisplay = "winsvr Self-Updating Agent"
	defaultDesc    = "Keeps a helper up to date and runs it in the active user session."
	defaultBin     = "helper.bin"
	defaultURL     = "" // e.g. https://downloads.example.com/helper.bin
)

var version = "dev"

func main() {
	var (
		name     = flag.String("name", defaultName, "service name (no spaces)")
		display  = flag.String("display", defaultDisplay, "service display name")
		desc     = flag.String("desc", defaultDesc, "service description")
		bin      = flag.String("bin", defaultBin, "payload file name, relative to this executable")
		url      = flag.String("url", defaultURL, "remote payload URL for auto-update (empty disables updates)")
		sidecar  = flag.Bool("sidecar", false, "use a <url>.json sidecar (version+crc64) instead of HEAD/ETag checks")
		interval = flag.Duration("interval", 0, "poll this often for a newer payload while it runs (0 = only between runs)")
		elevated = flag.Bool("elevated", false, "run the payload with the user's elevated (admin) token")
		hidden   = flag.Bool("hidden", true, "run the payload with no console window")
	)
	flag.Parse()

	cfg := winsvr.Config{
		Name:         *name,
		DisplayName:  *display,
		Description:  *desc,
		StartType:    winsvr.StartAutomatic,
		Account:      `NT AUTHORITY\LocalSystem`, // required to launch into a user session
		RestartDelay: 5 * time.Second,
		Arguments:    []string{"run"},
	}

	sup := &winsvr.Supervisor{
		Bin: *bin,
		Policy: winsvr.RunPolicy{
			Elevated:       *elevated,
			Hidden:         *hidden,
			UpdateInterval: *interval,
		},
	}
	if *url != "" {
		src := &update.Source{URL: *url}
		if *sidecar {
			src.Validator = &update.SidecarValidator{BinaryURL: *url}
		}
		sup.Update = src
	}

	cmd := "run"
	if flag.NArg() > 0 {
		cmd = strings.ToLower(flag.Arg(0))
	}

	if err := dispatch(cmd, cfg, sup); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dispatch(cmd string, cfg winsvr.Config, sup *winsvr.Supervisor) error {
	switch cmd {
	case "install":
		if err := selfElevate(); err != nil {
			return err
		}
		if err := winsvr.Install(cfg); err != nil {
			return err
		}
		fmt.Println("installed:", cfg.Name)
		if err := winsvr.Start(cfg.Name); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not start:", err)
		} else {
			fmt.Println("started:", cfg.Name)
		}
		return nil

	case "uninstall", "remove":
		if err := selfElevate(); err != nil {
			return err
		}
		if err := winsvr.Uninstall(cfg.Name); err != nil {
			return err
		}
		fmt.Println("uninstalled:", cfg.Name)
		return nil

	case "start":
		if err := selfElevate(); err != nil {
			return err
		}
		return winsvr.Start(cfg.Name)

	case "stop":
		if err := selfElevate(); err != nil {
			return err
		}
		return winsvr.Stop(cfg.Name)

	case "version":
		fmt.Println(cfg.Name, version)
		return nil

	case "status":
		st, err := winsvr.Status(cfg.Name)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", cfg.Name, st)
		return nil

	case "run":
		// Attach an event-log logger; harmless (no-op) off Windows.
		logger, closeLog, err := winsvr.NewEventLogger(cfg.Name)
		if err != nil {
			logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
		} else {
			defer closeLog()
		}
		sup.Logger = logger
		return winsvr.Run(cfg.Name, sup, cfg.StopTimeout)

	default:
		return fmt.Errorf("unknown command %q (install|uninstall|start|stop|status|run)", cmd)
	}
}

// selfElevate re-runs the current command elevated if it is not already, and
// exits the non-elevated process. On non-Windows it is a no-op.
func selfElevate() error {
	if elevate.IsElevated() {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := elevate.Relaunch(ctx, os.Args[1:])
	if err != nil {
		return err
	}
	os.Exit(int(code))
	return nil
}
