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
// can hold a freshly written file open for a moment. downloadAttempts bounds how
// many times a release changing mid-read is read again.
const (
	installAttempts   = 5
	installRetryDelay = 200 * time.Millisecond
	downloadAttempts  = 3
)

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
//   - default: an HTTP HEAD compares the ETag. The server must send one; see
//     EnsureLatest. Works with any static host (nginx, S3, a CDN) that is
//     configured to do so, with no other server changes.
//   - Sidecar: GET <URL>.json {"version","size","crc64"} (crc64 as hex), which
//     publishes the payload's checksum so it can be verified end to end.
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

// remote is what the server says about the payload right now. Only part of it is
// worth keeping across a restart; see updateState.
type remote struct {
	ETag    string // ETag mode: required, the only validator there is
	Version string // sidecar mode: informational, for the log
	Size    int64  // sidecar mode: declared size, checked after download
	CRC64   uint64 // sidecar mode: required, verified after download
}

// statePath is where ETag mode records what it installed. Sidecar mode writes
// nothing; see ensureSidecar.
func (u *Updater) statePath() string { return u.Path + ".update.json" }

// updateState is the little that has to survive a restart.
//
// The ETag is here because HTTP defines it as an opaque validator, so it cannot
// be assumed to follow from the payload's content. Some servers do make it a
// content hash: S3 uses the MD5 hex for a single-part upload. Plenty do not.
// nginx builds it from the modification time and the length, Apache from the
// modification time and the size, and an S3 multipart upload gives a digest of
// the part digests rather than of the file. Redeploying identical bytes changes
// the first two. So the value the server sent is recorded rather than derived.
//
// If you control the server and can publish a content hash, sidecar mode is that
// arrangement already, and it needs no state file at all.
//
// The checksum rides along so ETag mode can still notice a payload that changed
// locally while the remote stayed put.
type updateState struct {
	ETag  string `json:"etag,omitempty"`
	CRC64 uint64 `json:"crc64,omitempty"`
}

// EnsureLatest checks the remote source and, if it is newer or the local payload
// no longer matches what was installed, downloads, verifies, and installs it. It
// returns whether a download happened.
//
// Every failure is returned rather than worked around, including a server that
// sends no ETag in the default mode and a sidecar with no crc64. Both are
// misconfigurations that leave no way to tell a new release from the payload
// already on disk, so they are reported and the caller waits, rather than
// guessing from a weaker signal. Supervisor retries until the check succeeds.
func (u *Updater) EnsureLatest(ctx context.Context) (bool, error) {
	if u.Sidecar {
		return u.ensureSidecar(ctx)
	}
	return u.ensureETag(ctx)
}

// ensureETag keeps the payload in step with the ETag the server reports, which
// is the only thing it can compare, so the recorded one has to persist.
func (u *Updater) ensureETag(ctx context.Context) (bool, error) {
	cur := u.loadState()
	rem, err := u.head(ctx)
	if err != nil {
		return false, err
	}
	newer := rem.ETag != cur.ETag

	// The checksum recorded at install time is the only way to notice a payload
	// that was corrupted or replaced on disk while the remote stayed put. A state
	// file written before checksums were recorded has none, which reads as a
	// mismatch and costs one re-download before it is back in step.
	var localCRC uint64
	var haveLocal bool
	if !newer {
		localCRC, err = fileCRC64(u.Path)
		haveLocal = err == nil
	}
	changed := !newer && (!haveLocal || localCRC != cur.CRC64)

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

// ensureSidecar keeps the payload in step with <URL>.json, holding no state at
// all. The sidecar publishes the payload's own checksum, so comparing that with
// the file on disk answers both questions at once: whether a new release exists,
// and whether the payload is still the one that was installed.
func (u *Updater) ensureSidecar(ctx context.Context) (bool, error) {
	rem, err := u.sidecar(ctx)
	if err != nil {
		return false, err
	}
	localCRC, lerr := fileCRC64(u.Path)
	haveLocal := lerr == nil
	if haveLocal && localCRC == rem.CRC64 {
		u.logSidecar(rem, localCRC, haveLocal, "up-to-date")
		return false, nil
	}
	if _, err := u.download(ctx, rem); err != nil {
		return false, err
	}
	// Nothing reads a state file in this mode. Drop one left by ETag mode so it
	// cannot be mistaken for something that still matters.
	_ = os.Remove(u.statePath())
	u.logSidecar(rem, localCRC, haveLocal, "downloaded")
	return true, nil
}

func crcField(v uint64, have bool) string {
	if !have {
		return "absent"
	}
	return fmt.Sprintf("%016x", v)
}

func (u *Updater) logETag(cur updateState, rem remote, newer, changed bool, action string) {
	if u.Logger == nil {
		return
	}
	u.Logger.Info("update check",
		"mode", "etag",
		"path", u.Path,
		"remote_etag", rem.ETag,
		"local_etag", cur.ETag,
		"needsUpdate", newer,
		"locallyChanged", changed,
		"action", action)
}

func (u *Updater) logSidecar(rem remote, localCRC uint64, haveLocal bool, action string) {
	if u.Logger == nil {
		return
	}
	u.Logger.Info("update check",
		"mode", "sidecar",
		"path", u.Path,
		"remote_version", rem.Version,
		"remote_crc64", fmt.Sprintf("%016x", rem.CRC64),
		"local_crc64", crcField(localCRC, haveLocal),
		"needsUpdate", !haveLocal || localCRC != rem.CRC64,
		"action", action)
}

// head asks the server what the current payload is.
//
// An ETag is required. It is the only validator this mode has, and without one
// there is no way to tell a new release from the payload already installed.
// Falling back to Content-Length would compare a number that a recompression or
// a rebuild can leave unchanged, so a missing ETag is reported as the server
// misconfiguration it is.
func (u *Updater) head(ctx context.Context) (remote, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.URL, nil)
	if err != nil {
		return remote{}, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return remote{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A bad URL or a server that rejects HEAD lands here.
		return remote{}, fmt.Errorf("update: HEAD %s: %s", u.URL, resp.Status)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return remote{}, fmt.Errorf("update: HEAD %s: no ETag header, so the payload cannot be identified; configure the server to send one, or use sidecar mode", u.URL)
	}
	return remote{ETag: etag}, nil
}

func (u *Updater) sidecar(ctx context.Context) (remote, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.URL+".json", nil)
	if err != nil {
		return remote{}, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return remote{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return remote{}, fmt.Errorf("update: sidecar %s.json: %s", u.URL, resp.Status)
	}
	var sc struct {
		Version string `json:"version"`
		Size    int64  `json:"size"`
		CRC64   string `json:"crc64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sc); err != nil {
		return remote{}, fmt.Errorf("update: decode sidecar: %w", err)
	}
	crc, err := parseCRC(sc.CRC64)
	if err != nil {
		return remote{}, fmt.Errorf("update: sidecar crc64 %q: %w", sc.CRC64, err)
	}
	if crc == 0 {
		// Fail closed. The checksum is what this mode compares and what it
		// verifies, so a sidecar without one identifies nothing. Reading it as
		// "no verification needed" would let a server switch checking off.
		return remote{}, fmt.Errorf("update: sidecar %s.json has no crc64 field", u.URL)
	}
	return remote{Version: sc.Version, Size: sc.Size, CRC64: crc}, nil
}

// download fetches the payload, verifies it, and installs it at u.Path. It
// returns the state describing what landed on disk.
//
// In ETag mode the validator is recorded from the GET, not the earlier HEAD. A
// release landing between the two would otherwise store the old ETag against the
// new bytes, and the next genuine update would then read as up-to-date. When the
// two disagree the content changed under us, so the body is read again.
func (u *Updater) download(ctx context.Context, want remote) (updateState, error) {
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		tmpName, got, err := u.fetch(ctx, want)
		if err != nil {
			return updateState{}, err
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
			return updateState{}, err
		}
		return got, nil
	}
	return updateState{}, fmt.Errorf("update: %s changed on every read; gave up after %d attempts", u.URL, downloadAttempts)
}

// fetch downloads the payload into a temporary file beside u.Path and returns
// that file's name with the state describing the bytes it holds. The caller
// installs or discards the temporary file.
func (u *Updater) fetch(ctx context.Context, want remote) (string, updateState, error) {
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
	etag := resp.Header.Get("ETag")
	if !u.Sidecar && etag == "" {
		// Checked before reading the body, so a misconfigured server costs no
		// bandwidth. The GET's own ETag is what gets recorded, so it has to exist.
		return "", updateState{}, fmt.Errorf("update: GET %s: no ETag header, so the download cannot be identified; configure the server to send one, or use sidecar mode", u.URL)
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
	// Read one byte past the limit, so an oversized body is reported rather than
	// silently truncated into a corrupt payload.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("update: %s is larger than the %d byte limit", u.URL, limit)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	// Only the sidecar declares a size worth enforcing: it describes the payload
	// itself. A Content-Length does not, because transfer encoding can make the
	// bytes on the wire differ from the bytes written here.
	if err == nil && want.Size > 0 && n != want.Size {
		err = fmt.Errorf("update: size mismatch: got %d want %d", n, want.Size)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return "", updateState{}, err
	}

	sum, err := fileCRC64(tmpName)
	if err != nil {
		_ = os.Remove(tmpName)
		return "", updateState{}, fmt.Errorf("update: checksum the download: %w", err)
	}
	if want.CRC64 != 0 && sum != want.CRC64 {
		_ = os.Remove(tmpName)
		return "", updateState{}, fmt.Errorf("update: crc64 mismatch: got %016x want %016x", sum, want.CRC64)
	}
	// Recorded in both modes. ETag mode publishes no checksum, but storing the
	// one we installed is what lets a later local change be noticed.
	return tmpName, updateState{ETag: etag, CRC64: sum}, nil
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
