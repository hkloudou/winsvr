package winsvr

import (
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
