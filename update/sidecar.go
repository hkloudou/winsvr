package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Sidecar is the JSON document published next to the binary, e.g. at
// <binary-url>.json. Only the fields you populate are enforced.
type Sidecar struct {
	Version string `json:"version"`
	Size    int64  `json:"size"`
	// CRC64 is the CRC-64/ECMA checksum as a hex string ("a1b2...") or the
	// decimal form; both parse. 0/empty disables the checksum gate.
	CRC64 string `json:"crc64"`
}

// SidecarValidator fetches a Sidecar manifest and compares its Version (falling
// back to CRC64, then Size) to decide whether to update. On download it
// enforces the CRC-64/ECMA checksum end-to-end.
type SidecarValidator struct {
	// URL of the sidecar JSON. Defaults to BinaryURL + ".json" when empty.
	URL string
	// BinaryURL is only used to derive URL when URL is empty.
	BinaryURL string
	Client    *http.Client
}

func (s *SidecarValidator) url() string {
	if s.URL != "" {
		return s.URL
	}
	return s.BinaryURL + ".json"
}

func (s *SidecarValidator) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *SidecarValidator) Check(ctx context.Context, cur State) (bool, State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(), nil)
	if err != nil {
		return false, cur, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return false, cur, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, cur, fmt.Errorf("update: sidecar %s: status %s", s.url(), resp.Status)
	}
	var sc Sidecar
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return false, cur, fmt.Errorf("update: decode sidecar: %w", err)
	}
	crc, err := parseCRC(sc.CRC64)
	if err != nil {
		return false, cur, fmt.Errorf("update: sidecar crc64 %q: %w", sc.CRC64, err)
	}
	next := State{Version: sc.Version, Size: sc.Size, CRC64: crc}

	switch {
	case sc.Version != "" || cur.Version != "":
		return sc.Version != cur.Version, next, nil
	case crc != 0 || cur.CRC64 != 0:
		return crc != cur.CRC64, next, nil
	default:
		return sc.Size != cur.Size, next, nil
	}
}

// Verify enforces the sidecar CRC-64/ECMA checksum against the downloaded file.
func (s *SidecarValidator) Verify(path string, next State) error {
	return verifyCRC(path, next.CRC64)
}

func parseCRC(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	var v uint64
	// Try hex first, then decimal.
	if n, err := fmt.Sscanf(s, "%x", &v); err == nil && n == 1 {
		return v, nil
	}
	if n, err := fmt.Sscanf(s, "%d", &v); err == nil && n == 1 {
		return v, nil
	}
	return 0, fmt.Errorf("not a hex or decimal integer")
}
