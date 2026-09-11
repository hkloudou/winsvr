// Command example-helper is the payload ("helper.bin"): the program the service
// launches inside the logged-on user's desktop session. THIS IS WHERE YOUR REAL
// PROGRAM GOES — it runs as the interactive user, not as LocalSystem.
//
// This example writes a heartbeat file into the user's temp directory every few
// seconds, so you can confirm (by the file's owner) that it runs as the user.
// Replace main's body with your own work.
//
// Build it windowless so nothing flashes when the service starts it:
//
//	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
//	  -ldflags "-s -w -H windowsgui" -o helper.bin ./example/helper
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	// The service stops the payload by terminating it; also handle signals so
	// `helper.bin` behaves when run directly during development.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	heartbeat := filepath.Join(os.TempDir(), "winsvr-helper.txt")
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	write := func() {
		line := fmt.Sprintf("%s alive as %s pid=%d\n", time.Now().Format(time.RFC3339), os.Getenv("USERNAME"), os.Getpid())
		_ = os.WriteFile(heartbeat, []byte(line), 0o644)
	}
	write()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			write()
		}
	}
}
