package winsvr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServiceStateString(t *testing.T) {
	cases := map[ServiceState]string{Running: "running", Stopped: "stopped", StartPending: "start-pending"}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", uint32(s), got, want)
		}
	}
}

func TestParseCRC(t *testing.T) {
	for in, want := range map[string]uint64{"": 0, "ff": 255, "0xff": 255, "a1b2c3": 0xa1b2c3} {
		got, err := parseCRC(in)
		if err != nil || got != want {
			t.Errorf("parseCRC(%q) = %d,%v want %d", in, got, err, want)
		}
	}
	if _, err := parseCRC("zz"); err == nil {
		t.Error("parseCRC(zz) should error")
	}
}

func TestGrow(t *testing.T) {
	const s = 1_000_000_000 // 1s in ns
	cases := []struct{ cur, max, want int64 }{
		{2 * s, 30 * s, 4 * s},   // doubles
		{20 * s, 30 * s, 30 * s}, // caps at max
		{30 * s, 30 * s, 30 * s}, // stays at max
	}
	for _, c := range cases {
		if got := int64(grow(time.Duration(c.cur), time.Duration(c.max))); got != c.want {
			t.Errorf("grow(%d,%d)=%d want %d", c.cur, c.max, got, c.want)
		}
	}
}

// A session lookup that fails for any reason other than the service stopping is
// a real failure. Reporting it as a clean stop would hide it from the SCM, which
// then has no reason to restart the service. Off Windows the lookup reports
// ErrUnsupported, which stands in for that case.
func TestSupervisorReportsSessionLookupFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		// There the lookup waits for a real sign-in rather than reporting an
		// error, so Run would sit in its retry loop instead of returning and
		// this would hang rather than assert anything.
		t.Skip("the unexpected-error path is only reachable off Windows")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "helper.bin")
	if err := os.WriteFile(bin, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{Bin: "helper.bin", dir: dir} // no UpdateURL, so no network
	// Bounded so a future change cannot turn this into a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil; an unexpected session-lookup failure must be reported")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Run error = %v, want it to wrap ErrUnsupported", err)
	}
}

// A cancelled context is the ordinary stop path and must stay a clean exit.
func TestSupervisorCleanStopOnCancel(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "helper.bin")
	if err := os.WriteFile(bin, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Supervisor{Bin: "helper.bin", dir: dir}
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run on a cancelled context = %v, want nil", err)
	}
}
