package update

import (
	"context"
	"crypto/subtle"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// crcTable is the ECMA polynomial table, matching the reference implementation.
var crcTable = crc64.MakeTable(crc64.ECMA)

// FileCRC64 returns the CRC-64/ECMA checksum of the file at path, or 0 if it
// cannot be read (e.g. the file does not exist yet).
func FileCRC64(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	h := crc64.New(crcTable)
	if _, err := io.Copy(h, f); err != nil {
		return 0
	}
	return h.Sum64()
}

// State is the validator-defined identity of the currently installed binary
// (an ETag string, a version, a checksum...). It is persisted between runs so
// the next check is a single HEAD.
type State struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Size         int64  `json:"size,omitempty"`
	Version      string `json:"version,omitempty"`
	CRC64        uint64 `json:"crc64,omitempty"`
}

// Validator decides whether the remote binary differs from the local State,
// and validates a freshly downloaded file.
type Validator interface {
	// Check performs a cheap remote probe (typically HTTP HEAD) and reports
	// whether an update is available, along with the State to persist once the
	// new binary is installed.
	Check(ctx context.Context, cur State) (newer bool, next State, err error)
	// Verify checks a downloaded file against next (the State returned by
	// Check). It may be a no-op for header-only validators.
	Verify(path string, next State) error
}

// Source describes what to keep updated and how.
type Source struct {
	// URL is the remote binary. Required.
	URL string
	// Validator decides when to download. Defaults to HTTPValidator{URL: URL}.
	Validator Validator
	// Client is the HTTP client to use. Defaults to a client with Timeout.
	Client *http.Client
	// Timeout bounds each HTTP request. Defaults to 30s.
	Timeout time.Duration
}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	to := s.Timeout
	if to <= 0 {
		to = 30 * time.Second
	}
	return &http.Client{Timeout: to}
}

func (s *Source) validator() Validator {
	if s.Validator != nil {
		return s.Validator
	}
	return &HTTPValidator{URL: s.URL, Client: s.client()}
}

// Updater keeps a single local file in sync with Source.
type Updater struct {
	Source Source
	// Path is the local binary path.
	Path string
	// StatePath is where the persisted State lives. Defaults to Path + ".state".
	StatePath string
}

func (u *Updater) statePath() string {
	if u.StatePath != "" {
		return u.StatePath
	}
	return u.Path + ".state"
}

// Result reports what EnsureLatest did.
type Result struct {
	Updated bool  // a new binary was downloaded and installed
	State   State // the State now persisted
}

// EnsureLatest checks the remote source and, if a newer binary is available (or
// the local file is missing), downloads it, verifies it, and atomically swaps
// it into place. A network failure during Check is returned to the caller so it
// can decide whether to proceed with the existing binary.
func (u *Updater) EnsureLatest(ctx context.Context) (Result, error) {
	v := u.Source.validator()
	cur := u.loadState()

	_, missing := os.Stat(u.Path)
	newer, next, err := v.Check(ctx, cur)
	if err != nil {
		return Result{State: cur}, err
	}
	if !newer && missing == nil {
		return Result{Updated: false, State: cur}, nil
	}

	if err := u.download(ctx, next, v); err != nil {
		return Result{State: cur}, err
	}
	u.saveState(next)
	return Result{Updated: true, State: next}, nil
}

func (u *Updater) download(ctx context.Context, next State, v Validator) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.Source.URL, nil)
	if err != nil {
		return err
	}
	resp, err := u.Source.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: GET %s: unexpected status %s", u.Source.URL, resp.Status)
	}

	dir := filepath.Dir(u.Path)
	tmp, err := os.CreateTemp(dir, filepath.Base(u.Path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := v.Verify(tmpName, next); err != nil {
		return fmt.Errorf("update: downloaded file failed verification: %w", err)
	}
	// Atomic on the same volume. On Windows os.Rename fails if the destination
	// exists, so remove it first (the old binary must not be running).
	_ = os.Remove(u.Path)
	if err := os.Rename(tmpName, u.Path); err != nil {
		return fmt.Errorf("update: install: %w", err)
	}
	return nil
}

// verifyCRC compares the file's CRC-64/ECMA against want using a
// constant-time-ish comparison (defense in depth; CRC is not cryptographic).
func verifyCRC(path string, want uint64) error {
	if want == 0 {
		return nil
	}
	got := FileCRC64(path)
	var a, b [8]byte
	for i := 0; i < 8; i++ {
		a[i] = byte(got >> (8 * i))
		b[i] = byte(want >> (8 * i))
	}
	if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		return fmt.Errorf("crc64 mismatch: got %016x want %016x", got, want)
	}
	return nil
}

// Available reports whether the remote source is newer than the persisted
// State, using only a cheap probe (no download). Use it for periodic polling
// while the payload is running; call EnsureLatest to actually install.
func (u *Updater) Available(ctx context.Context) (bool, error) {
	newer, _, err := u.Source.validator().Check(ctx, u.loadState())
	return newer, err
}
