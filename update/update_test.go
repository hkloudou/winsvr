package update

import (
	"context"
	"fmt"
	"hash/crc64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFileCRC64(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	body := []byte("hello winsvr")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	want := crc64.Checksum(body, crc64.MakeTable(crc64.ECMA))
	if got := FileCRC64(p); got != want {
		t.Fatalf("FileCRC64 = %x, want %x", got, want)
	}
	if got := FileCRC64(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("missing file CRC = %x, want 0", got)
	}
}

func TestHTTPValidatorETag(t *testing.T) {
	etag := `"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Method == http.MethodGet {
			w.Write([]byte("payload-v1"))
		}
	}))
	defer srv.Close()

	v := &HTTPValidator{URL: srv.URL}
	newer, next, err := v.Check(context.Background(), State{})
	if err != nil {
		t.Fatal(err)
	}
	if !newer || next.ETag != etag {
		t.Fatalf("first check: newer=%v etag=%q", newer, next.ETag)
	}
	// Same ETag -> not newer.
	newer, _, err = v.Check(context.Background(), State{ETag: etag})
	if err != nil {
		t.Fatal(err)
	}
	if newer {
		t.Fatal("expected no update when ETag unchanged")
	}
}

func TestEnsureLatestDownloadsAndSwaps(t *testing.T) {
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
	binPath := filepath.Join(dir, "helper.bin")
	u := &Updater{Source: Source{URL: srv.URL}, Path: binPath}

	res, err := u.EnsureLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated {
		t.Fatal("expected Updated=true on first run")
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != body {
		t.Fatalf("payload = %q, want %q", got, body)
	}
	if _, err := os.Stat(u.statePath()); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
	// Second run: ETag unchanged and file present -> no update.
	res, err = u.EnsureLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Fatal("expected no update on second run")
	}
}

func TestSidecarValidatorCRC(t *testing.T) {
	const body = "abc123"
	crc := crc64.Checksum([]byte(body), crc64.MakeTable(crc64.ECMA))
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"1.2.3","size":%d,"crc64":"%x"}`, len(body), crc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	binURL := srv.URL + "/helper.bin"
	dir := t.TempDir()
	u := &Updater{
		Source: Source{URL: binURL, Validator: &SidecarValidator{BinaryURL: binURL}},
		Path:   filepath.Join(dir, "helper.bin"),
	}
	res, err := u.EnsureLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.State.Version != "1.2.3" {
		t.Fatalf("res = %+v", res)
	}
}

func TestSidecarCRCMismatchRejected(t *testing.T) {
	const body = "abc123"
	mux := http.NewServeMux()
	mux.HandleFunc("/helper.bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	mux.HandleFunc("/helper.bin.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"1.2.3","crc64":"dead"}`) // wrong crc
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	binURL := srv.URL + "/helper.bin"
	dir := t.TempDir()
	u := &Updater{
		Source: Source{URL: binURL, Validator: &SidecarValidator{BinaryURL: binURL}},
		Path:   filepath.Join(dir, "helper.bin"),
	}
	if _, err := u.EnsureLatest(context.Background()); err == nil {
		t.Fatal("expected verification error on CRC mismatch")
	}
	if _, err := os.Stat(u.Path); err == nil {
		t.Fatal("corrupt payload must not be installed")
	}
}
