package winsvr

import "testing"

func TestServiceStateString(t *testing.T) {
	for s, want := range map[ServiceState]string{
		Running: "running", Stopped: "stopped", StartPending: "start-pending",
	} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", uint32(s), got, want)
		}
	}
}
