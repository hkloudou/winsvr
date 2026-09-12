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

// defaultTimeout bounds one whole update request, reading the body included. Set
// Updater.Client to lift it and rely on the context instead.
const defaultTimeout = 5 * time.Minute

// Bounds on the two retry loops: the final rename, which a scanner can block for
// a moment, and re-reading a release that changed mid-download.
const (
	installAttempts   = 5
	installRetryDelay = 200 * time.Millisecond
	downloadAttempts  = 3
)

// staleTempAge is how old an abandoned download has to be before a sweep removes
// it. Well above defaultTimeout, so a transfer still in flight is never taken out
// from under itself.
const staleTempAge = time.Hour

// fileCRC64 returns the CRC-64/ECMA checksum of a file. An unreadable or absent
// file is an error, never a checksum of zero, so a read failure cannot be
// mistaken for a valid digest.
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

// Updater keeps one local file in sync with a remote URL. This library checks it
// once, at service start (see Supervisor). Two modes:
//
//   - default: an HTTP HEAD compares the ETag, which the server must send.
//   - Sidecar: GET <URL>.json {"version","size","crc64"} (crc64 as hex),
//     publishing the payload's checksum so it can be verified end to end.
//
// Neither mode guesses. A server that cannot identify its payload is reported, and
// the payload does not launch. CRC-64 finds corruption, not tampering: verify a
// signature yourself if you need authenticity. The README covers both.
type Updater struct {
	URL     string       // remote binary (required)
	Path    string       // local file (required)
	Sidecar bool         // use <URL>.json instead of HEAD/ETag
	Client  *http.Client // defaults to a 5-minute client that refuses TLS downgrades
	// MaxBytes caps the download size (default DefaultMaxBytes). A larger
	// response is rejected rather than written to disk.
	MaxBytes int64
	// Logger, when set, receives one Info "update check" record per check with
	// the validators compared, whether an update was needed, and what was done.
	Logger *slog.Logger
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: defaultTimeout, CheckRedirect: refuseTLSDowngrade}
}

// refuseTLSDowngrade stops an https update URL from being steered onto plain http
// by a redirect, which would hand the payload to anyone on the path. Go's default
// policy follows such a redirect.
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

// statePath is where ETag mode records what it installed. Sidecar mode writes
// nothing; see ensureSidecar.
func (u *Updater) statePath() string { return u.Path + ".update.json" }

// payloadInfo describes a payload. Only the tagged fields are written to
// statePath; the rest come from the server on every check. The ETag is stored
// because HTTP defines it as opaque, so it cannot be derived from the payload.
// The README says which servers make it a content hash and which do not.
type payloadInfo struct {
	ETag  string `json:"etag,omitempty"`  // ETag mode: required, the only validator there is
	CRC64 uint64 `json:"crc64,omitempty"` // what was installed, so a local change shows up

	Version string `json:"-"` // sidecar: informational, for the log
	Size    int64  `json:"-"` // sidecar: declared size, checked after download
}

// EnsureLatest checks the remote source and, if it is newer or the local payload
// no longer matches what was installed, downloads, verifies, and installs it. It
// returns whether a download happened. Every failure is returned rather than
// worked around.
func (u *Updater) EnsureLatest(ctx context.Context) (bool, error) {
	u.sweepTemps()
	if u.Sidecar {
		return u.ensureSidecar(ctx)
	}
	return u.ensureETag(ctx)
}

// sweepTemps removes downloads abandoned beside the payload. Every error path in
// a download cleans up after itself, but a process killed mid-transfer, by a
// service stop or a power cut, cannot, and nothing else would ever remove what it
// left. Only files older than staleTempAge go, so a concurrent download survives.
func (u *Updater) sweepTemps() {
	dir := filepath.Dir(u.Path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(u.Path) + ".tmp-"
	cutoff := time.Now().Add(-staleTempAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr == nil && u.Logger != nil {
			u.Logger.Info("update: removed an abandoned download", "path", e.Name())
		}
	}
}

// ensureETag keeps the payload in step with the ETag the server reports, which is
// all it can compare, so the recorded one has to persist.
func (u *Updater) ensureETag(ctx context.Context) (bool, error) {
	cur := u.loadState()
	rem, err := u.head(ctx)
	if err != nil {
		return false, err
	}
	newer := rem.ETag != cur.ETag
	// The recorded checksum is the only way to notice a payload corrupted or
	// replaced on disk while the remote stayed put, so the file is read only when
	// the remote says nothing changed. A state file predating checksums has none,
	// which reads as a change and costs one re-download to get back in step.
	changed := !newer && !u.matchesInstalled(cur.CRC64)

	if !newer && !changed {
		u.logETag(cur, rem, newer, changed, "up-to-date")
		return false, nil
	}
	installed, err := u.download(ctx, rem)
	if err != nil {
		return false, err
	}
	if serr := u.saveState(installed); serr != nil && u.Logger != nil {
		u.Logger.Warn("update: could not record state; the next start will download again",
			"path", u.statePath(), "err", serr)
	}
	u.logETag(cur, rem, newer, changed, "downloaded")
	return true, nil
}

// matchesInstalled reports whether the payload on disk is still the one whose
// checksum was recorded. A missing or unreadable file does not match.
func (u *Updater) matchesInstalled(want uint64) bool {
	got, err := fileCRC64(u.Path)
	return err == nil && got == want
}

// ensureSidecar keeps the payload in step with <URL>.json, holding no state at
// all. The sidecar publishes the payload's own checksum, so comparing that with
// the file on disk answers both questions at once: whether a new release exists,
// and whether the payload is still the one that was installed.
func (u *Updater) ensureSidecar(ctx context.Context) (bool, error) {
	rem, err := u.sidecar(ctx)
	if err != nil {
		return false, err
	}
	local, lerr := fileCRC64(u.Path)
	if lerr == nil && local == rem.CRC64 {
		u.logSidecar(rem, local, true, "up-to-date")
		return false, nil
	}
	if _, err := u.download(ctx, rem); err != nil {
		return false, err
	}
	// Nothing reads a state file here. Drop one left by ETag mode so it cannot be
	// mistaken for something that still matters.
	_ = os.Remove(u.statePath())
	u.logSidecar(rem, local, lerr == nil, "downloaded")
	return true, nil
}

func (u *Updater) logETag(cur, rem payloadInfo, newer, changed bool, action string) {
	if u.Logger == nil {
		return
	}
	u.Logger.Info("update check", "mode", "etag", "path", u.Path,
		"remote_etag", rem.ETag,
		"local_etag", cur.ETag,
		"needsUpdate", newer,
		"locallyChanged", changed,
		"action", action)
}

func (u *Updater) logSidecar(rem payloadInfo, local uint64, haveLocal bool, action string) {
	if u.Logger == nil {
		return
	}
	localCRC := "absent"
	if haveLocal {
		localCRC = fmt.Sprintf("%016x", local)
	}
	u.Logger.Info("update check", "mode", "sidecar", "path", u.Path,
		"remote_version", rem.Version,
		"remote_crc64", fmt.Sprintf("%016x", rem.CRC64),
		"local_crc64", localCRC,
		"needsUpdate", !haveLocal || local != rem.CRC64,
		"action", action)
}

// noETag reports a server that does not identify its payload. Falling back to
// Content-Length would compare a number a rebuild can leave unchanged, so this is
// an error rather than a guess.
func noETag(method, url string) error {
	return fmt.Errorf("update: %s %s: no ETag header, so the payload cannot be identified; set one on the server, or use sidecar mode", method, url)
}

// head asks the server what the current payload is.
func (u *Updater) head(ctx context.Context) (payloadInfo, error) {
	resp, err := u.do(ctx, http.MethodHead, u.URL)
	if err != nil {
		return payloadInfo{}, err
	}
	defer resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return payloadInfo{}, noETag(http.MethodHead, u.URL)
	}
	return payloadInfo{ETag: etag}, nil
}

func (u *Updater) sidecar(ctx context.Context) (payloadInfo, error) {
	resp, err := u.do(ctx, http.MethodGet, u.URL+".json")
	if err != nil {
		return payloadInfo{}, err
	}
	defer resp.Body.Close()
	var sc struct {
		Version string `json:"version"`
		Size    int64  `json:"size"`
		CRC64   string `json:"crc64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return payloadInfo{}, fmt.Errorf("update: decode sidecar: %w", err)
	}
	crc, err := parseCRC(sc.CRC64)
	if err != nil {
		return payloadInfo{}, fmt.Errorf("update: sidecar crc64 %q: %w", sc.CRC64, err)
	}
	if crc == 0 {
		// Fail closed. The checksum is what this mode compares and what it
		// verifies, so a sidecar without one identifies nothing. Reading it as
		// "nothing to verify" would let a server switch checking off.
		return payloadInfo{}, fmt.Errorf("update: sidecar %s.json has no crc64 field", u.URL)
	}
	return payloadInfo{Version: sc.Version, Size: sc.Size, CRC64: crc}, nil
}

// do issues one request and rejects anything but 200, so a bad URL or a server
// that refuses the method is reported plainly. The caller closes the body.
func (u *Updater) do(ctx context.Context, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("update: %s %s: %s", method, url, resp.Status)
	}
	return resp, nil
}

// download fetches the payload, verifies it, and installs it at u.Path, returning
// what landed on disk. In ETag mode the validator comes from the GET, not the
// earlier HEAD: a release landing between the two would store the old ETag against
// the new bytes, so the next genuine update would read as up-to-date. When they
// disagree the body is read again.
func (u *Updater) download(ctx context.Context, want payloadInfo) (payloadInfo, error) {
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		tmpName, got, err := u.fetch(ctx, want)
		if err != nil {
			return payloadInfo{}, err
		}
		if !u.Sidecar && got.ETag != want.ETag {
			_ = os.Remove(tmpName)
			if u.Logger != nil {
				u.Logger.Warn("update: remote changed between HEAD and GET; reading again",
					"attempt", attempt, "head_etag", want.ETag, "get_etag", got.ETag)
			}
			want.ETag = got.ETag
			continue
		}
		if err := u.install(ctx, tmpName); err != nil {
			_ = os.Remove(tmpName)
			return payloadInfo{}, err
		}
		return got, nil
	}
	return payloadInfo{}, fmt.Errorf("update: %s changed on every read; gave up after %d attempts", u.URL, downloadAttempts)
}

// fetch downloads the payload into a temporary file beside u.Path and returns
// that file's name with what describes the bytes it holds. The caller installs or
// discards the temporary file.
func (u *Updater) fetch(ctx context.Context, want payloadInfo) (string, payloadInfo, error) {
	resp, err := u.do(ctx, http.MethodGet, u.URL)
	if err != nil {
		return "", payloadInfo{}, err
	}
	defer resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if !u.Sidecar && etag == "" {
		// Checked before the body is read, so a misconfigured server costs no
		// bandwidth. The GET's own ETag is what gets recorded, so it has to exist.
		return "", payloadInfo{}, noETag(http.MethodGet, u.URL)
	}
	limit := u.maxBytes()
	if resp.ContentLength > limit {
		return "", payloadInfo{}, fmt.Errorf("update: %s declares %d bytes, over the %d byte limit", u.URL, resp.ContentLength, limit)
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Path), filepath.Base(u.Path)+".tmp-*")
	if err != nil {
		return "", payloadInfo{}, err
	}
	tmpName := tmp.Name()
	fail := func(err error) (string, payloadInfo, error) {
		_ = os.Remove(tmpName)
		return "", payloadInfo{}, err
	}

	// Read one byte past the limit, so an oversized body is reported rather than
	// silently truncated into a corrupt payload.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("update: %s is larger than the %d byte limit", u.URL, limit)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	// Only the sidecar declares a size worth enforcing: it describes the payload
	// itself. A Content-Length does not, because transfer encoding can make the
	// bytes on the wire differ from the bytes written here.
	if want.Size > 0 && n != want.Size {
		return fail(fmt.Errorf("update: size mismatch: got %d want %d", n, want.Size))
	}
	sum, err := fileCRC64(tmpName)
	if err != nil {
		return fail(fmt.Errorf("update: checksum the download: %w", err))
	}
	if want.CRC64 != 0 && sum != want.CRC64 {
		return fail(fmt.Errorf("update: crc64 mismatch: got %016x want %016x", sum, want.CRC64))
	}
	// The checksum is recorded in both modes. ETag mode publishes none, but
	// storing the one installed is what lets a later local change be noticed.
	return tmpName, payloadInfo{ETag: etag, CRC64: sum}, nil
}

// install puts the downloaded file in place. The existing payload is removed
// first so the rename cannot be refused over a destination already there, then
// the rename is retried briefly: giving up on the first transient lock would
// leave no payload at all.
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

func (u *Updater) loadState() payloadInfo {
	var s payloadInfo
	if data, err := os.ReadFile(u.statePath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func (u *Updater) saveState(s payloadInfo) error {
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
