package winsvr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// The elevation branch is the part of an elevated launch that can be reasoned
// about without Windows, so it is the part worth pinning. The rest needs a real
// session and is left to manual testing.
func TestChooseElevation(t *testing.T) {
	cases := []struct {
		name       string
		kind       uint32
		isElevated bool
		want       elevationChoice
	}{
		{"administrator under UAC holds the filtered half", elevationLimited, false, elevateWithLinked},
		{"a limited token is never used as is", elevationLimited, true, elevateWithLinked},
		{"an already elevated token needs nothing", elevationFull, true, elevateWithToken},
		{"full wins even if the elevation check disagrees", elevationFull, false, elevateWithToken},
		{"UAC off, administrator: the token is already full", elevationDefault, true, elevateWithToken},
		{"standard user: no elevated token exists", elevationDefault, false, elevateImpossible},
		{"an unknown type falls back to the elevation check", 99, true, elevateWithToken},
		{"an unknown type on a plain token is refused", 99, false, elevateImpossible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chooseElevation(c.kind, c.isElevated); got != c.want {
				t.Errorf("chooseElevation(%d, %v) = %d, want %d", c.kind, c.isElevated, got, c.want)
			}
		})
	}
}

// The standard-user case must stay distinguishable from the ordinary "nobody is
// signed in yet" wait, because one is a configuration mistake and the other is
// not. Supervisor.Run branches on exactly this.
func TestElevationErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrNoElevatedToken, ErrNoUserSession) || errors.Is(ErrNoUserSession, ErrNoElevatedToken) {
		t.Fatal("the two waiting/configuration errors must not match each other")
	}
	wrapped := fmt.Errorf("launch into session 3: %w", ErrNoElevatedToken)
	if !errors.Is(wrapped, ErrNoElevatedToken) {
		t.Error("ErrNoElevatedToken must survive wrapping; Supervisor.Run tests it with errors.Is")
	}
	if errors.Is(wrapped, ErrNoUserSession) {
		t.Error("a wrapped ErrNoElevatedToken must not read as ErrNoUserSession")
	}
}

// The auto-elevate retry fires on exactly one failure and at most once, so both
// halves are worth pinning: it must not treat other launch failures as a reason
// to raise privilege, and it must not loop.
func TestShouldAutoElevate(t *testing.T) {
	required := fmt.Errorf("launch: %w", ErrElevationRequired)
	cases := []struct {
		name            string
		err             error
		alreadyElevated bool
		disabled        bool
		want            bool
	}{
		{"the payload said it needs administrator", required, false, false, true},
		{"bare sentinel, not wrapped", ErrElevationRequired, false, false, true},
		{"never twice in one run", required, true, false, false},
		{"switched off by the deployment", required, false, true, false},
		{"off and already elevated", required, true, true, false},
		{"any other failure is not a reason to raise privilege", errors.New("access denied"), false, false, false},
		{"a missing session is not a reason either", ErrNoUserSession, false, false, false},
		{"nor is a user who has no elevated token", ErrNoElevatedToken, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAutoElevate(c.err, c.alreadyElevated, c.disabled); got != c.want {
				t.Errorf("shouldAutoElevate(%v, %v, %v) = %v, want %v",
					c.err, c.alreadyElevated, c.disabled, got, c.want)
			}
		})
	}
}

// Auto-elevate is on when the field is left alone, which is what makes the zero
// value mean "default true" despite Go's zero value being false.
func TestAutoElevateDefaultsOn(t *testing.T) {
	var s Supervisor
	if s.DisableAutoElevate {
		t.Fatal("the zero value must leave auto-elevate enabled")
	}
	if s.LaunchElevated {
		t.Fatal("the zero value must not launch elevated up front")
	}
}

type panicky struct{}

func (panicky) Run(context.Context) error { panic("boom") }

type fine struct{ err error }

func (f fine) Run(context.Context) error { return f.err }

// A panic in Run would end the process, and as a service its stderr goes
// nowhere, so the cause has to come back as an error that can be logged.
func TestRunRecoveredTurnsAPanicIntoAnError(t *testing.T) {
	err := runRecovered(context.Background(), panicky{})
	if err == nil {
		t.Fatal("a panic must come back as an error, not escape")
	}
	msg := err.Error()
	if !strings.Contains(msg, "boom") {
		t.Errorf("the panic value is missing from %q", msg)
	}
	if !strings.Contains(msg, "panicky.Run") {
		t.Error("the stack is missing; without it the error says what but not where")
	}
	if len(msg) > maxPanicStack+256 {
		t.Errorf("message is %d bytes, over what one event-log record can hold", len(msg))
	}
}

func TestRunRecoveredNilService(t *testing.T) {
	if err := runRecovered(context.Background(), nil); err == nil {
		t.Fatal("a nil Service must be reported, not crash the process")
	}
}

func TestRunRecoveredPassesThroughOrdinaryResults(t *testing.T) {
	if err := runRecovered(context.Background(), fine{}); err != nil {
		t.Errorf("a clean return became %v", err)
	}
	want := errors.New("ordinary failure")
	if err := runRecovered(context.Background(), fine{want}); !errors.Is(err, want) {
		t.Errorf("an ordinary error came back as %v", err)
	}
}

// Doubling before comparing overflowed into a negative duration, and a negative
// sleep returns at once.
func TestGrowNeverGoesNegative(t *testing.T) {
	for _, cur := range []time.Duration{math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64 - 1, math.MaxInt64} {
		if got := grow(cur, math.MaxInt64); got <= 0 {
			t.Errorf("grow(%d, MaxInt64) = %d, went non-positive", cur, got)
		}
	}
}
