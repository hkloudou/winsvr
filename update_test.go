package winsvr

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
