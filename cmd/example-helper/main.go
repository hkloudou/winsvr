// Command example-helper is a stand-in payload ("helper.bin"): the program the
// winsvr-agent service launches inside the logged-on user's desktop session.
//
// It writes a heartbeat file into the current user's temp directory every few
// seconds, so you can confirm it is running as the interactive user (check the
// file's owner) and not as LocalSystem. Replace it with your real workload.
//
// Build it as a windowless binary so no console flashes when the service starts
// it, and manifest it "asInvoker" so it can run as a standard user:
//
//	go-winres simply --manifest gui --arch amd64,386 --out cmd/example-helper/rsrc \
//	  --product-name helper --file-version git-tag --product-version git-tag
//	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
//	  -ldflags "-s -w -H windowsgui" -o helper.bin ./cmd/example-helper
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	ctx := make(chan os.Signal, 1)
	signal.Notify(ctx, os.Interrupt, syscall.SIGTERM)

	path := filepath.Join(os.TempDir(), "winsvr-helper-heartbeat.txt")
	user := os.Getenv("USERNAME")
	if user == "" {
		user = "?"
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	write := func() {
		line := fmt.Sprintf("%s alive as %s pid=%d version=%s\n", time.Now().Format(time.RFC3339), user, os.Getpid(), version)
		_ = os.WriteFile(path, []byte(line), 0o644)
	}
	write()
	for {
		select {
		case <-ctx:
			return
		case <-tick.C:
			write()
		}
	}
}
