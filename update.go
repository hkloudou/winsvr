package winsvr

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var crcTable = crc64.MakeTable(crc64.ECMA)

// fileCRC64 returns the CRC-64/ECMA checksum of a file, or 0 if unreadable.
func fileCRC64(path string) uint64 {
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

// Updater keeps one local file in sync with a remote URL. This library checks
// it once, at service start (see Supervisor). Two modes:
//
//   - default: an HTTP HEAD compares ETag, then Content-Length. Works with any
//     static host (nginx, S3, a CDN) with no server changes.
//   - Sidecar: GET <URL>.json {"version","size","crc64"} (crc64 as hex) to compare a version
//     and verify a CRC-64/ECMA checksum end-to-end after download.
type Updater struct {
	URL     string       // remote binary (required)
	Path    string       // local file (required)
	Sidecar bool         // use <URL>.json instead of HEAD/ETag
	Client  *http.Client // defaults to a 30s client
	// Logger, when set, receives one Info "update check" record per check with
	// the remote and local validators (ETag, or version+crc64 in sidecar mode),
	// whether an update was needed, and the action taken. Supervisor passes its
	// own logger here.
	Logger *slog.Logger
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (u *Updater) statePath() string { return u.Path + ".update.json" }

type updateState struct {
	ETag    string `json:"etag,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Version string `json:"version,omitempty"`
	CRC64   uint64 `json:"crc64,omitempty"`
}

// EnsureLatest checks the remote source and, if it is newer (or the local file
// is missing), downloads, verifies, and atomically installs it. It returns
// whether a download happened. A network error is returned so the caller can
// decide whether to proceed with the existing file.
func (u *Updater) EnsureLatest(ctx context.Context) (updated bool, err error) {
	cur := u.loadState()
	var newer bool
	var next updateState
	if u.Sidecar {
		newer, next, err = u.checkSidecar(ctx, cur)
	} else {
		newer, next, err = u.checkHTTP(ctx, cur)
	}
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(u.Path)
	missing := statErr != nil
	if !newer && !missing {
		u.logCheck(cur, next, newer, missing, "up-to-date")
		return false, nil
	}
	if err := u.download(ctx, next); err != nil {
		return false, err
	}
	u.saveState(next)
	u.logCheck(cur, next, newer, missing, "downloaded")
	return true, nil
}

// logCheck emits one Info record describing the comparison and the action, with
// mode-appropriate fields (ETag for HTTP, version+crc64 for sidecar). No-op
// when no Logger is set.
func (u *Updater) logCheck(cur, next updateState, needsUpdate, missing bool, action string) {
	if u.Logger == nil {
		return
	}
	if u.Sidecar {
		u.Logger.Info("update check",
			"mode", "sidecar",
			"path", u.Path,
			"remote_version", next.Version,
			"local_version", cur.Version,
			"remote_crc64", fmt.Sprintf("%016x", next.CRC64),
			"local_crc64", fmt.Sprintf("%016x", cur.CRC64),
			"needsUpdate", needsUpdate,
			"missing", missing,
			"action", action)
		return
	}
	u.Logger.Info("update check",
		"mode", "http",
		"path", u.Path,
		"remote_etag", next.ETag,
		"local_etag", cur.ETag,
		"needsUpdate", needsUpdate,
		"missing", missing,
		"action", action)
}

func (u *Updater) checkHTTP(ctx context.Context, cur updateState) (bool, updateState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.URL, nil)
	if err != nil {
		return false, cur, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return false, cur, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A bad URL or a server that rejects HEAD lands here; surface it clearly
		// instead of misreading a missing ETag as "a newer payload".
		return false, cur, fmt.Errorf("update: HEAD %s: %s", u.URL, resp.Status)
	}
	next := updateState{ETag: resp.Header.Get("ETag")}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		next.Size, _ = strconv.ParseInt(cl, 10, 64)
	}
	if next.ETag != "" || cur.ETag != "" {
		return next.ETag != cur.ETag, next, nil
	}
	return next.Size != cur.Size, next, nil
}

func (u *Updater) checkSidecar(ctx context.Context, cur updateState) (bool, updateState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL+".json", nil)
	if err != nil {
		return false, cur, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return false, cur, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, cur, fmt.Errorf("update: sidecar %s.json: %s", u.URL, resp.Status)
	}
	var sc struct {
		Version string `json:"version"`
		Size    int64  `json:"size"`
		CRC64   string `json:"crc64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return false, cur, fmt.Errorf("update: decode sidecar: %w", err)
	}
	crc, err := parseCRC(sc.CRC64)
	if err != nil {
		return false, cur, fmt.Errorf("update: sidecar crc64 %q: %w", sc.CRC64, err)
	}
	next := updateState{Version: sc.Version, Size: sc.Size, CRC64: crc}
	if sc.Version != "" || cur.Version != "" {
		return sc.Version != cur.Version, next, nil
	}
	if crc != 0 || cur.CRC64 != 0 {
		return crc != cur.CRC64, next, nil
	}
	return sc.Size != cur.Size, next, nil
}

func (u *Updater) download(ctx context.Context, next updateState) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL, nil)
	if err != nil {
		return err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: GET %s: %s", u.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Path), filepath.Base(u.Path)+".tmp-*")
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
	if next.CRC64 != 0 {
		if got := fileCRC64(tmpName); got != next.CRC64 {
			return fmt.Errorf("update: crc64 mismatch: got %016x want %016x", got, next.CRC64)
		}
	}
	_ = os.Remove(u.Path) // Windows can't rename onto an existing file
	if err := os.Rename(tmpName, u.Path); err != nil {
		return fmt.Errorf("update: install: %w", err)
	}
	return nil
}

func (u *Updater) loadState() updateState {
	var s updateState
	if data, err := os.ReadFile(u.statePath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func (u *Updater) saveState(s updateState) {
	if data, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(u.statePath(), data, 0o644)
	}
}

// parseCRC reads a CRC-64/ECMA checksum written as hex (optionally 0x-prefixed),
// e.g. "a1b2c3d4e5f60718". Hex is required so values are unambiguous.
func parseCRC(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("not a hex integer")
	}
	return v, nil
}
