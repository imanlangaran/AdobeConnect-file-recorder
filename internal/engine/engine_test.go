package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
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

// TestDecoupledDownloadAndReconstruct verifies DownloadArchive and ReconstructArchive
// can be invoked independently.
func TestDecoupledDownloadAndReconstruct(t *testing.T) {
	var body bytes.Buffer
	zw := zip.NewWriter(&body)
	w, err := zw.Create("dummy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "dummy content"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body.Bytes())
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "rec.zip")
	opt := Options{
		ZipURL:    srv.URL + "/rec.zip",
		Title:     "rec",
		OutDir:    t.TempDir(),
		CachePath: cachePath,
	}

	// 1. DownloadArchive step.
	zipPath, size, err := DownloadArchive(context.Background(), opt)
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if zipPath != cachePath {
		t.Fatalf("DownloadArchive path: got %q, want %q", zipPath, cachePath)
	}
	if size != int64(body.Len()) {
		t.Fatalf("DownloadArchive size: got %d, want %d", size, body.Len())
	}

	// 2. ReconstructArchive step using downloaded zipPath.
	// Fails cleanly with ErrUnsupported because archive has no mainstream.xml.
	_, err = ReconstructArchive(context.Background(), opt, zipPath)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ReconstructArchive: want ErrUnsupported, got %v", err)
	}

	// 3. DownloadArchive with ZipPath short-circuits.
	p, sz, err := DownloadArchive(context.Background(), Options{ZipPath: zipPath})
	if err != nil || p != zipPath || sz != size {
		t.Fatalf("DownloadArchive with ZipPath: got (%q, %d, %v), want (%q, %d, nil)", p, sz, err, zipPath, size)
	}
}
