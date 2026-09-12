package winsvr

import (
	"context"
	"encoding/json"
	"errors"
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

// DefaultMaxBytes caps a payload download, so a broken or hostile server cannot
// fill the disk. Override it with Updater.MaxBytes.
const DefaultMaxBytes int64 = 512 << 20 // 512 MiB

// defaultTimeout bounds one whole update request, reading the body included. A
// payload can be tens of megabytes on a slow link, so this is deliberately
// generous; set Updater.Client to lift it and rely on the context instead.
const defaultTimeout = 5 * time.Minute

// installAttempts is how many times an install retries the final rename, and
// installRetryDelay is the pause between tries. A scanner or the search indexer
// can hold a freshly written file open for a moment.
const (
	installAttempts   = 5
	installRetryDelay = 200 * time.Millisecond
	downloadAttempts  = 3
)

// fileCRC64 returns the CRC-64/ECMA checksum of a file. An unreadable file is
// an error, never a checksum of zero, so a read failure cannot be mistaken for
// a valid digest.
func fileCRC64(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc64.New(crcTable)
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

// Updater keeps one local file in sync with a remote URL. This library checks
// it once, at service start (see Supervisor). Two modes:
//
//   - default: an HTTP HEAD compares ETag, then Content-Length. Works with any
//     static host (nginx, S3, a CDN) with no server changes.
//   - Sidecar: GET <URL>.json {"version","size","crc64"} (crc64 as hex) to compare a version
//     and verify a CRC-64/ECMA checksum end-to-end after download.
//
// Note that CRC-64 detects accidental corruption, not tampering: it is not a
// cryptographic hash, and the sidecar travels over the same connection as the
// payload. Serve both over HTTPS from a host you trust, and verify a signature
// yourself if the payload needs authenticity rather than integrity.
type Updater struct {
	URL     string       // remote binary (required)
	Path    string       // local file (required)
	Sidecar bool         // use <URL>.json instead of HEAD/ETag
	Client  *http.Client // defaults to a 5-minute client that refuses TLS downgrades
	// MaxBytes caps the download size (default DefaultMaxBytes). A larger
	// response is rejected rather than written to disk.
	MaxBytes int64
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
	return &http.Client{Timeout: defaultTimeout, CheckRedirect: refuseTLSDowngrade}
}

// refuseTLSDowngrade stops an https update URL from being steered onto plain
// http by a redirect, which would otherwise hand the payload to anyone on the
// path. Go's default policy follows such a redirect.
func refuseTLSDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("update: refusing redirect from https to %s", req.URL.Scheme)
	}
	return nil
}

func (u *Updater) maxBytes() int64 {
	if u.MaxBytes > 0 {
		return u.MaxBytes
	}
	return DefaultMaxBytes
}

func (u *Updater) statePath() string { return u.Path + ".update.json" }

type updateState struct {
	ETag    string `json:"etag,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Version string `json:"version,omitempty"`
	CRC64   uint64 `json:"crc64,omitempty"`
}

// EnsureLatest checks the remote source and, if it is newer (or the local file
// is missing or no longer matches the checksum recorded for it), downloads,
// verifies, and installs it. It returns whether a download happened. A network
// error is returned so the caller can decide whether to proceed with the
// existing file.
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

	// The recorded checksum describes the bytes that were installed, so
	// re-verify the file itself. Without this a payload that was corrupted or
	// replaced on disk after install is trusted forever, because the remote
	// validator still matches the state file.
	corrupt := false
	if !missing && cur.CRC64 != 0 {
		got, cerr := fileCRC64(u.Path)
		if cerr != nil || got != cur.CRC64 {
			corrupt = true
		}
	}

	if !newer && !missing && !corrupt {
		u.logCheck(cur, next, newer, missing, corrupt, "up-to-date")
		return false, nil
	}
	installed, err := u.download(ctx, next)
	if err != nil {
		return false, err
	}
	if serr := u.saveState(installed); serr != nil && u.Logger != nil {
		u.Logger.Warn("update: could not record state; the next start will download again",
			"path", u.statePath(), "err", serr)
	}
	u.logCheck(cur, installed, newer, missing, corrupt, "downloaded")
	return true, nil
}

// logCheck emits one Info record describing the comparison and the action, with
// mode-appropriate fields (ETag for HTTP, version+crc64 for sidecar). No-op
// when no Logger is set.
func (u *Updater) logCheck(cur, next updateState, needsUpdate, missing, corrupt bool, action string) {
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
			"corrupt", corrupt,
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
		"corrupt", corrupt,
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
	if crc == 0 {
		// Fail closed. The sidecar exists to carry an integrity check, so a
		// missing crc64 is a broken sidecar. Treating it as "no verification
		// needed" would let a server silently turn checking off.
		return false, cur, fmt.Errorf("update: sidecar %s.json has no crc64 field", u.URL)
	}
	next := updateState{Version: sc.Version, Size: sc.Size, CRC64: crc}
	if sc.Version != "" || cur.Version != "" {
		return sc.Version != cur.Version, next, nil
	}
	return crc != cur.CRC64, next, nil
}

// download fetches the payload, verifies it, and installs it at u.Path. It
// returns the state describing what actually landed on disk.
//
// The validator is recorded from the GET, not from the earlier HEAD. A release
// that lands between the two would otherwise store the old ETag against the new
// bytes, and the next genuine update would then be read as "up-to-date". When
// the two disagree the content changed under us, so the body is read again.
func (u *Updater) download(ctx context.Context, want updateState) (updateState, error) {
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		tmpName, got, err := u.fetch(ctx, want)
		if err != nil {
			return updateState{}, err
		}
		if !u.Sidecar && want.ETag != "" && got.ETag != "" && got.ETag != want.ETag {
			_ = os.Remove(tmpName)
			if u.Logger != nil {
				u.Logger.Warn("update: remote changed between HEAD and GET; reading again",
					"attempt", attempt, "head_etag", want.ETag, "get_etag", got.ETag)
			}
			want = got
			continue
		}
		if err := u.install(ctx, tmpName); err != nil {
			_ = os.Remove(tmpName)
			return updateState{}, err
		}
		return got, nil
	}
	return updateState{}, fmt.Errorf("update: %s changed on every read; gave up after %d attempts", u.URL, downloadAttempts)
}

// fetch downloads the payload into a temporary file beside u.Path and returns
// that file's name with the validators the server reported for the bytes it
// served. The caller installs or discards the temporary file.
func (u *Updater) fetch(ctx context.Context, want updateState) (string, updateState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL, nil)
	if err != nil {
		return "", updateState{}, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return "", updateState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", updateState{}, fmt.Errorf("update: GET %s: %s", u.URL, resp.Status)
	}
	limit := u.maxBytes()
	if resp.ContentLength > limit {
		return "", updateState{}, fmt.Errorf("update: %s declares %d bytes, over the %d byte limit", u.URL, resp.ContentLength, limit)
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Path), filepath.Base(u.Path)+".tmp-*")
	if err != nil {
		return "", updateState{}, err
	}
	tmpName := tmp.Name()
	// Read one byte past the limit, so an oversized body is reported rather
	// than silently truncated into a corrupt payload.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("update: %s is larger than the %d byte limit", u.URL, limit)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && want.Size > 0 && n != want.Size {
		err = fmt.Errorf("update: size mismatch: got %d want %d", n, want.Size)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return "", updateState{}, err
	}

	got := updateState{ETag: resp.Header.Get("ETag"), Size: n, Version: want.Version}
	sum, err := fileCRC64(tmpName)
	if err != nil {
		_ = os.Remove(tmpName)
		return "", updateState{}, fmt.Errorf("update: checksum the download: %w", err)
	}
	if want.CRC64 != 0 && sum != want.CRC64 {
		_ = os.Remove(tmpName)
		return "", updateState{}, fmt.Errorf("update: crc64 mismatch: got %016x want %016x", sum, want.CRC64)
	}
	// Record the checksum in both modes. HTTP mode publishes none, but storing
	// what we installed is what lets EnsureLatest notice a later local change.
	got.CRC64 = sum
	return tmpName, got, nil
}

// install puts the downloaded file in place. The existing payload is removed
// first so the rename cannot be refused over a destination that is already
// there, and the rename is then retried briefly: giving up on the first
// transient lock would leave no payload at all.
func (u *Updater) install(ctx context.Context, tmpName string) error {
	_ = os.Remove(u.Path)
	var err error
	for attempt := 1; attempt <= installAttempts; attempt++ {
		if err = os.Rename(tmpName, u.Path); err == nil {
			return nil
		}
		if attempt == installAttempts || !sleep(ctx, installRetryDelay) {
			break
		}
	}
	return fmt.Errorf("update: install %s: %w", u.Path, err)
}

func (u *Updater) loadState() updateState {
	var s updateState
	if data, err := os.ReadFile(u.statePath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func (u *Updater) saveState(s updateState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(u.statePath(), data, 0o644)
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
