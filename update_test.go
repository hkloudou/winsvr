package winsvr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdaterHEAD(t *testing.T) {
	const body = "payload-v1"
	etag := `"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, body)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}

	updated, err := u.EnsureLatest(context.Background())
	if err != nil || !updated {
		t.Fatalf("first EnsureLatest = %v,%v", updated, err)
	}
	if got, _ := os.ReadFile(u.Path); string(got) != body {
		t.Fatalf("payload = %q", got)
	}
	updated, err = u.EnsureLatest(context.Background())
	if err != nil || updated {
		t.Fatalf("second EnsureLatest = %v,%v (want false)", updated, err)
	}
}

func TestUpdaterHEADNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("expected an error for a non-200 HEAD")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Fatal("nothing should be installed on a failed check")
	}
}

func TestUpdaterLogsETagDecision(t *testing.T) {
	etag := `"v9"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, "body")
		}
	}))
	defer srv.Close()

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin"), Logger: logger}

	if _, err := u.EnsureLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"update check", "remote_etag", "local_etag", "needsUpdate=", "action=downloaded"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got: %s", want, out)
		}
	}
}

// A release landing between the HEAD and the GET used to be recorded under the
// HEAD's validator, so the next check read a genuinely new payload as current.
func TestUpdaterRecordsETagFromGET(t *testing.T) {
	var headETag atomic.Value
	headETag.Store(`"v1"`)
	const getETag = `"v2"`
	var gets atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("ETag", headETag.Load().(string))
			return
		}
		gets.Add(1)
		w.Header().Set("ETag", getETag)
		fmt.Fprint(w, "payload-v2")
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}
	updated, err := u.EnsureLatest(context.Background())
	if err != nil || !updated {
		t.Fatalf("EnsureLatest = %v,%v", updated, err)
	}
	if got := gets.Load(); got != 2 {
		t.Errorf("GET count = %d, want 2: a HEAD/GET disagreement must be read again", got)
	}
	if st := u.loadState(); st.ETag != getETag {
		t.Fatalf("recorded etag = %q, want the GET's %q", st.ETag, getETag)
	}
	// The server has settled. HEAD now agrees with the bytes on disk, so the
	// next check must do nothing; recording the HEAD's etag would re-download.
	headETag.Store(getETag)
	updated, err = u.EnsureLatest(context.Background())
	if err != nil || updated {
		t.Fatalf("second EnsureLatest = %v,%v (want false)", updated, err)
	}
}

// The remote validator still matches, so only the checksum recorded at install
// time can reveal that the payload was swapped on disk.
func TestUpdaterReplacesTamperedPayload(t *testing.T) {
	const body = "payload-v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, body)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}
	if _, err := u.EnsureLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.Path, []byte("malicious"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, err := u.EnsureLatest(context.Background())
	if err != nil || !updated {
		t.Fatalf("EnsureLatest after tampering = %v,%v, want a re-download", updated, err)
	}
	if got, _ := os.ReadFile(u.Path); string(got) != body {
		t.Fatalf("payload = %q, want the remote copy restored", got)
	}
}

func TestUpdaterRejectsOversizePayload(t *testing.T) {
	big := strings.Repeat("x", 4096)

	t.Run("declared length", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"big"`)
			if r.Method == http.MethodGet {
				fmt.Fprint(w, big)
			}
		}))
		defer srv.Close()
		dir := t.TempDir()
		u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin"), MaxBytes: 1024}
		if _, err := u.EnsureLatest(context.Background()); err == nil {
			t.Fatal("a body over MaxBytes must be rejected")
		}
		if _, err := os.Stat(u.Path); err == nil {
			t.Fatal("nothing should be installed")
		}
	})

	t.Run("chunked", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"big"`)
			if r.Method != http.MethodGet {
				return
			}
			// Flushing early drops Content-Length, so the cap has to hold while
			// the body is being read rather than from the header alone.
			fmt.Fprint(w, "start")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, big)
		}))
		defer srv.Close()
		dir := t.TempDir()
		u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin"), MaxBytes: 1024}
		if _, err := u.EnsureLatest(context.Background()); err == nil {
			t.Fatal("an unbounded body over MaxBytes must be rejected while reading")
		}
		if _, err := os.Stat(u.Path); err == nil {
			t.Fatal("nothing should be installed")
		}
	})
}

func TestRefuseTLSDowngrade(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	secure := req("https://dl.example.com/helper.bin")

	if err := refuseTLSDowngrade(req("http://evil.example.com/x"), []*http.Request{secure}); err == nil {
		t.Error("a redirect from https to http must be refused")
	}
	if err := refuseTLSDowngrade(req("https://cdn.example.com/x"), []*http.Request{secure}); err != nil {
		t.Errorf("https to https must be allowed: %v", err)
	}
	// An http origin was never protected, so do not invent a new failure there.
	plain := req("http://dl.example.com/helper.bin")
	if err := refuseTLSDowngrade(req("http://other.example.com/x"), []*http.Request{plain}); err != nil {
		t.Errorf("http to http must be allowed: %v", err)
	}
	var via []*http.Request
	for i := 0; i < 10; i++ {
		via = append(via, secure)
	}
	if err := refuseTLSDowngrade(req("https://cdn.example.com/x"), via); err == nil {
		t.Error("the redirect limit must still be enforced")
	}
}

func TestFileCRC64MissingFileErrors(t *testing.T) {
	if _, err := fileCRC64(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("an unreadable file must be an error, never a checksum of zero")
	}
}

// A failed install must not leave the payload missing: the state is not saved,
// so the next service start downloads again.
func TestUpdaterFailedInstallLeavesNoState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, "payload")
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	// A non-empty directory at the payload path cannot be removed and cannot be
	// renamed over, so the install fails on every platform. An empty one would
	// not do: os.Remove deletes that happily.
	path := filepath.Join(dir, "helper.bin")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	u := &Updater{URL: srv.URL, Path: path}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("an install that cannot complete must report an error")
	}
	if _, err := os.Stat(u.statePath()); err == nil {
		t.Fatal("no state may be recorded for an install that did not happen")
	}
	// No temporary files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}

// The ETag is all there is to compare, so a server that does not send one is
// reported rather than worked around.
func TestUpdaterRequiresETag(t *testing.T) {
	t.Run("missing on HEAD", func(t *testing.T) {
		var gets atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				gets.Add(1)
				fmt.Fprint(w, "payload")
			}
		}))
		defer srv.Close()

		dir := t.TempDir()
		u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}
		if _, err := u.EnsureLatest(context.Background()); err == nil {
			t.Fatal("a HEAD with no ETag must be an error, not a fallback comparison")
		}
		if got := gets.Load(); got != 0 {
			t.Errorf("GET count = %d, want 0: nothing should be downloaded", got)
		}
		if _, err := os.Stat(u.Path); err == nil {
			t.Error("nothing should be installed")
		}
	})

	t.Run("missing on GET", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("ETag", `"v1"`)
				return
			}
			fmt.Fprint(w, "payload") // no ETag on the body
		}))
		defer srv.Close()

		dir := t.TempDir()
		u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}
		if _, err := u.EnsureLatest(context.Background()); err == nil {
			t.Fatal("a GET with no ETag must be an error: it is what gets recorded")
		}
		if _, err := os.Stat(u.Path); err == nil {
			t.Error("nothing should be installed")
		}
	})

}

// ETag mode cannot recompute the remote validator from the file, so it does keep
// a state file, holding only what cannot be derived.
func TestUpdaterETagKeepsState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, "payload-v1")
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}
	if _, err := u.EnsureLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(u.statePath())
	if err != nil {
		t.Fatalf("etag mode must record the remote validator: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"version", "size"} {
		if _, ok := raw[gone]; ok {
			t.Errorf("state still carries %q, which is derivable or unused", gone)
		}
	}
	if raw["etag"] != `"v1"` {
		t.Errorf("state etag = %v, want the served one", raw["etag"])
	}
	if _, ok := raw["crc64"]; !ok {
		t.Error("state must record the installed checksum, to notice a local change")
	}
}

// A process killed mid-download cannot clean up after itself, and nothing else
// used to remove what it left beside the payload.
func TestUpdaterSweepsAbandonedDownloads(t *testing.T) {
	const body = "payload-v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodGet {
			fmt.Fprint(w, body)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL, Path: filepath.Join(dir, "helper.bin")}

	stale := filepath.Join(dir, "helper.bin.tmp-999999")
	fresh := filepath.Join(dir, "helper.bin.tmp-111111")
	other := filepath.Join(dir, "unrelated.tmp-222222")
	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Only the stale one is old enough to be swept.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := u.EnsureLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("an abandoned download older than the cutoff must be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent temporary file may belong to a download in flight; it must be left alone")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("a file that is not this payload's temporary must be left alone")
	}
	if got, _ := os.ReadFile(u.Path); string(got) != body {
		t.Fatalf("payload = %q, want %q", got, body)
	}
}

// install removes the existing payload before renaming the download over it, which
// is deliberate. This pins what that costs, so the behaviour cannot drift
// unnoticed: when the rename cannot be made, the old payload is gone too, and no
// state is recorded, so the next check downloads again.
//
// The other failed-install test cannot reach this window, because its fixture is a
// non-empty directory on which the remove itself fails.
func TestInstallRemovesThePayloadBeforeItKnowsTheRenameWorks(t *testing.T) {
	dir := t.TempDir()
	u := &Updater{URL: "http://example.invalid/helper.bin", Path: filepath.Join(dir, "helper.bin")}
	if err := os.WriteFile(u.Path, []byte("the old payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A download that is not there can never be renamed into place.
	if err := u.install(context.Background(), filepath.Join(dir, "helper.bin.tmp-404")); err == nil {
		t.Fatal("install with no downloaded file must report an error")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Fatal("the payload survived: the remove-first window is gone, so update install's doc comment")
	}
	if _, err := os.Stat(u.statePath()); err == nil {
		t.Error("no state may be recorded for an install that did not happen")
	}
}

// The sweep must only touch names os.CreateTemp could have produced, so a file
// someone put there by hand is left alone.
func TestSweepOnlyMatchesGeneratedTempNames(t *testing.T) {
	const prefix = "helper.bin.tmp-"
	for name, want := range map[string]bool{
		"helper.bin.tmp-123456":         true,
		"helper.bin.tmp-0":              true,
		"helper.bin.tmp-4294967295":     true,  // the largest uint32
		"helper.bin.tmp-4294967296":     false, // one past it
		"helper.bin.tmp-20260912123456": false, // a timestamp-shaped backup
		"helper.bin.tmp-007":            false, // leading zeroes
		"helper.bin.tmp-backup":         false,
		"helper.bin.tmp-":               false,
		"helper.bin.tmp-12a":            false,
		"helper.bin.tmp-12.old":         false,
		"helper.bin.tmp--1":             false,
		"helper.bin.tmp-+1":             false,
		"helper.bin.tmp- 12":            false,
		"helper.bin":                    false,
		"helper.bin.update.json":        false,
		"otherpayload.tmp-123":          false,
	} {
		if got := isGeneratedTemp(name, prefix); got != want {
			t.Errorf("isGeneratedTemp(%q) = %v, want %v", name, got, want)
		}
	}
}

// math.MaxInt64 is the natural way to ask for no size limit. Reading one byte
// past it used to wrap to a negative count, which read nothing, installed an
// empty payload, and reported success.
func TestUpdaterNoLimitStillReadsTheBody(t *testing.T) {
	const body = "the real payload"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodGet {
			w.(http.Flusher).Flush() // no Content-Length, as a chunked CDN response
			fmt.Fprint(w, body)
		}
	}))
	defer srv.Close()

	u := &Updater{URL: srv.URL, Path: filepath.Join(t.TempDir(), "helper.bin"), MaxBytes: math.MaxInt64}
	if _, err := u.EnsureLatest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(u.Path); string(got) != body {
		t.Fatalf("installed %q, want %q", got, body)
	}
}

// An empty payload can never run, and once installed it could never be
// replaced: its checksum is zero, which is also what an absent checksum reads
// back as, so every later check would call it up to date.
func TestUpdaterRefusesAnEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`) // a valid validator, and nothing behind it
	}))
	defer srv.Close()

	u := &Updater{URL: srv.URL, Path: filepath.Join(t.TempDir(), "helper.bin")}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("an empty body must be refused")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Error("nothing may be installed from an empty body")
	}
	if _, err := os.Stat(u.statePath()); err == nil {
		t.Error("no state may be recorded, or the empty payload would stick")
	}
}
