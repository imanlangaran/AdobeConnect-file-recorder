package download

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileSuccess(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("BREEZESESSION")
		if err == nil {
			gotCookie = c.Value
		}
		w.Header().Set("Content-Length", "4")
		w.Write([]byte("PK\x03\x04"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.zip")
	var progressed bool
	err := File(context.Background(), srv.Client(), srv.URL, dest, Options{
		Session:  "tok123",
		Validate: ValidateZIP,
		Progress: func(d, t int64) { progressed = true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotCookie != "tok123" {
		t.Errorf("cookie = %q", gotCookie)
	}
	if !progressed {
		t.Error("no progress callback")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "PK\x03\x04" {
		t.Errorf("content = %q", b)
	}
	if _, err := os.Stat(dest + ".part"); err == nil {
		t.Error(".part left behind")
	}
}

func TestFileRejectsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<!DOCTYPE html><html>login</html>"))
	}))
	defer srv.Close()
	err := File(context.Background(), srv.Client(), srv.URL,
		filepath.Join(t.TempDir(), "x.zip"), Options{Validate: ValidateZIP})
	if !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}

func TestFileAuthRejectedNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	err := File(context.Background(), srv.Client(), srv.URL,
		filepath.Join(t.TempDir(), "x.zip"), Options{Attempts: 3})
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (auth errors must not retry)", calls)
	}
}

func TestFileRetriesServerErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte("PK\x03\x04data"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "r.zip")
	err := File(context.Background(), srv.Client(), srv.URL, dest,
		Options{Attempts: 3, Validate: ValidateZIP})
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestFileCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("PK\x03\x04"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(5 * time.Second) // hold body open
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := File(ctx, srv.Client(), srv.URL,
		filepath.Join(t.TempDir(), "c.zip"), Options{Attempts: 1})
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context cancel", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("cancel took too long: %s", time.Since(start))
	}
}

func TestValidateZIPVariants(t *testing.T) {
	if err := ValidateZIP([]byte("PK\x03\x04rest")); err != nil {
		t.Errorf("zip rejected: %v", err)
	}
	for _, bad := range []string{"<!DOCTYPE html>", "<html>", "<?xml version", "XX\x00\x00"} {
		if err := ValidateZIP([]byte(bad)); err == nil {
			t.Errorf("ValidateZIP(%q) should fail", bad)
		} else if strings.Contains(bad, "<") && !errors.Is(err, ErrNoContent) {
			t.Errorf("err = %v, want ErrNoContent", err)
		}
	}
}
