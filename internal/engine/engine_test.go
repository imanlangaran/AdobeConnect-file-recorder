package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestCachePathSkipsRedownload: a retried Run with the same CachePath (the
// serve-mode case after a reconstruct failure) must not hit the server again.
func TestCachePathSkipsRedownload(t *testing.T) {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	if err := zw.Close(); err != nil { // valid but empty archive
		t.Fatal(err)
	}
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(body.Bytes())
	}))
	defer srv.Close()

	opt := Options{
		ZipURL:    srv.URL + "/rec.zip",
		Title:     "rec",
		OutDir:    t.TempDir(),
		CachePath: filepath.Join(t.TempDir(), "cache", "task1.zip"),
	}
	// Empty archive fails extraction (after the download) -> retry scenario.
	if _, err := Run(context.Background(), opt); err == nil {
		t.Fatal("first Run: want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("first Run: server hits = %d, want 1", got)
	}

	if _, err := Run(context.Background(), opt); err == nil {
		t.Fatal("second Run: want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("second Run: server hits = %d, want 1 (cache must skip the download)", got)
	}
}

// TestNoCachePathAlwaysDownloads: without CachePath behavior is unchanged.
func TestNoCachePathAlwaysDownloads(t *testing.T) {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(body.Bytes())
	}))
	defer srv.Close()

	opt := Options{ZipURL: srv.URL + "/rec.zip", Title: "rec", OutDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), opt); err == nil {
			t.Fatalf("Run %d: want error, got nil", i+1)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (no cache -> download every run)", got)
	}
}
