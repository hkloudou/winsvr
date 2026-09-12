package winsvr

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"log/slog"
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

func TestUpdaterSidecarCRC(t *testing.T) {
	const body = "abc123"
	crc := crc64.Checksum([]byte(body), crc64.MakeTable(crc64.ECMA))
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"1.2.3","size":%d,"crc64":"%x"}`, len(body), crc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL + "/helper.bin", Path: filepath.Join(dir, "helper.bin"), Sidecar: true}
	updated, err := u.EnsureLatest(context.Background())
	if err != nil || !updated {
		t.Fatalf("EnsureLatest = %v,%v", updated, err)
	}
}

func TestUpdaterSidecarCRCMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "abc123") })
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"1.2.3","crc64":"dead"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL + "/helper.bin", Path: filepath.Join(dir, "helper.bin"), Sidecar: true}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("expected CRC mismatch error")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Fatal("corrupt payload must not be installed")
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

func TestUpdaterSidecarRequiresCRC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "abc123") })
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"1.2.3"}`) // no crc64 at all
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL + "/helper.bin", Path: filepath.Join(dir, "helper.bin"), Sidecar: true}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("a sidecar with no crc64 must fail closed, not silently skip verification")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Fatal("nothing should be installed from an incomplete sidecar")
	}
}

func TestUpdaterSidecarSizeMismatch(t *testing.T) {
	const body = "abc123"
	crc := crc64.Checksum([]byte(body), crc64.MakeTable(crc64.ECMA))
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"1.2.3","size":999,"crc64":"%x"}`, crc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL + "/helper.bin", Path: filepath.Join(dir, "helper.bin"), Sidecar: true}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("a declared size that does not match the body must be rejected")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Fatal("nothing should be installed on a size mismatch")
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

// The default mode has nothing but the ETag to compare, so a server that does
// not send one is reported rather than worked around.
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

	// Sidecar mode identifies the payload by its published checksum, so it needs
	// no ETag at all.
	t.Run("not required in sidecar mode", func(t *testing.T) {
		const body = "payload"
		crc := crc64.Checksum([]byte(body), crc64.MakeTable(crc64.ECMA))
		mux := http.NewServeMux()
		mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"version":"1.0.0","size":%d,"crc64":"%x"}`, len(body), crc)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		dir := t.TempDir()
		u := &Updater{URL: srv.URL + "/helper.bin", Path: filepath.Join(dir, "helper.bin"), Sidecar: true}
		if updated, err := u.EnsureLatest(context.Background()); err != nil || !updated {
			t.Fatalf("EnsureLatest = %v,%v", updated, err)
		}
	})
}

// Sidecar mode compares the published checksum against the file on disk, so the
// state file ETag mode needs is redundant there: none is written, and a leftover
// one is cleaned up.
func TestUpdaterSidecarKeepsNoState(t *testing.T) {
	const body = "payload-v1"
	crc := crc64.Checksum([]byte(body), crc64.MakeTable(crc64.ECMA))
	var gets atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"1.2.3","size":%d,"crc64":"%x"}`, len(body), crc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	u := &Updater{URL: srv.URL + "/helper.bin", Path: filepath.Join(dir, "helper.bin"), Sidecar: true}
	// Leave behind the state file ETag mode would have written.
	if err := os.WriteFile(u.statePath(), []byte(`{"etag":"\"stale\""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if updated, err := u.EnsureLatest(context.Background()); err != nil || !updated {
		t.Fatalf("EnsureLatest = %v,%v", updated, err)
	}
	if _, err := os.Stat(u.statePath()); err == nil {
		t.Error("sidecar mode must not leave a state file behind")
	}

	// With no state at all the second check is still a no-op: the comparison
	// comes from the payload itself.
	if updated, err := u.EnsureLatest(context.Background()); err != nil || updated {
		t.Fatalf("second EnsureLatest = %v,%v (want false)", updated, err)
	}
	if got := gets.Load(); got != 1 {
		t.Errorf("payload GET count = %d, want 1", got)
	}

	// The same comparison catches a payload changed on disk.
	if err := os.WriteFile(u.Path, []byte("malicious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if updated, err := u.EnsureLatest(context.Background()); err != nil || !updated {
		t.Fatalf("EnsureLatest after tampering = %v,%v, want a re-download", updated, err)
	}
	if got, _ := os.ReadFile(u.Path); string(got) != body {
		t.Fatalf("payload = %q, want the published copy restored", got)
	}
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
