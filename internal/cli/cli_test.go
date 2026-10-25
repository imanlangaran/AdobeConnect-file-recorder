package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseResolution(t *testing.T) {
	for _, want := range []resOption{
		{"480p", 854, 480},
		{"720p", 1280, 720},
		{"1080p", 1920, 1080},
	} {
		got, err := parseResolution(want.name)
		if err != nil {
			t.Fatalf("parseResolution(%q): %v", want.name, err)
		}
		if got != want {
			t.Errorf("parseResolution(%q) = %+v, want %+v", want.name, got, want)
		}
		if got.w%2 != 0 || got.h%2 != 0 {
			t.Errorf("%q dims must be even for yuv420p: %dx%d", got.name, got.w, got.h)
		}
	}
}

func TestParseResolutionRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"1080i", "4k", "1366x768", "480", ""} {
		_, err := parseResolution(bad)
		if err == nil {
			t.Errorf("parseResolution(%q) accepted invalid input", bad)
			continue
		}
		// the error must teach the valid options
		for _, opt := range []string{"480p", "720p", "1080p"} {
			if !strings.Contains(err.Error(), opt) {
				t.Errorf("error for %q must list %q: %v", bad, opt, err)
			}
		}
	}
}

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	os.WriteFile(path, []byte(
		"# credentials\n\nAC_USERNAME=alice\nAC_PASSWORD=\"s3cr3t\"\n"), 0o600)
	u, p, err := readEnvFile(path)
	if err != nil || u != "alice" || p != "s3cr3t" {
		t.Errorf("readEnvFile = %q, %q, %v", u, p, err)
	}

	// plain USERNAME/PASSWORD keys, export prefix, single quotes
	os.WriteFile(path, []byte("export USERNAME=bob\nPASSWORD='pw'\n"), 0o600)
	if u, p, err = readEnvFile(path); err != nil || u != "bob" || p != "pw" {
		t.Errorf("fallback keys: %q, %q, %v", u, p, err)
	}

	// missing file → no auth, no error
	if u, p, err = readEnvFile(filepath.Join(dir, "nope.env")); err != nil || u != "" || p != "" {
		t.Errorf("missing file: %q, %q, %v", u, p, err)
	}

	// file without credentials → clear error
	os.WriteFile(path, []byte("UNRELATED=1\n"), 0o600)
	if _, _, err = readEnvFile(path); err == nil {
		t.Error("missing credentials must be an error")
	}
}

// TestFetchTitle: the page title (entity-encoded, behind login) becomes the
// output name; the login page itself must never be used as a title.
func TestFetchTitle(t *testing.T) {
	const want = "تصویر پردازی رقمی (صدقی یزدی - گروه 200)_0"
	// numeric character references, exactly as Adobe Connect serves them
	var encoded strings.Builder
	for _, r := range want {
		fmt.Fprintf(&encoded, "&#%d;", r)
	}
	loggedIn := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/xml": // login endpoint
			if r.URL.Query().Get("action") != "login" || r.URL.Query().Get("password") != "pw" {
				fmt.Fprint(w, `<results><status code="denied"/></results>`)
				return
			}
			loggedIn = true
			http.SetCookie(w, &http.Cookie{Name: "BREEZESESSION", Value: "s1"})
			fmt.Fprint(w, `<results><status code="ok"/></results>`)
		case "/rec/":
			c, err := r.Cookie("BREEZESESSION")
			if err != nil || c.Value != "s1" { // unauthenticated → login page
				fmt.Fprint(w, `<html><head><title>Adobe Connect Central Login</title></head>`+
					`<body><form action="/system/login-guest?account-id=7"></form></body></html>`)
				return
			}
			fmt.Fprintf(w, "<html><head><title>%s</title></head></html>", encoded.String())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	// no creds → login page → no title (fallback happens at the call site)
	if got := fetchTitle(ctx, srv.URL+"/rec/", "", nil, "", ""); got != "" {
		t.Errorf("unauthenticated: got %q, want empty", got)
	}
	// bad creds → login refused → still no title
	if got := fetchTitle(ctx, srv.URL+"/rec/", "", nil, "u", "wrong"); got != "" {
		t.Errorf("bad creds: got %q, want empty", got)
	}
	// good creds → logged in → decoded title
	got := fetchTitle(ctx, srv.URL+"/rec/", "", nil, "u", "pw")
	if !loggedIn {
		t.Fatal("fetchTitle did not log in with credentials")
	}
	if got != want {
		t.Errorf("fetchTitle = %q, want %q", got, want)
	}
}

// TestFetchTitleStripsAdobeConnectSuffix: platform chrome is not part of the name.
func TestFetchTitleStripsAdobeConnectSuffix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><head><title>Week 3 | Adobe Connect</title></head></html>")
	}))
	defer srv.Close()
	if got := fetchTitle(context.Background(), srv.URL+"/", "", nil, "", ""); got != "Week 3" {
		t.Errorf("fetchTitle = %q, want %q", got, "Week 3")
	}
}

// TestFetchTitleLive checks the real recording page. Skipped unless
// AC_LIVE_URL is set (e.g. AC_LIVE_URL=https://connect.example.com/meeting12345678/)
// because it needs network + credentials from ./.env.
func TestFetchTitleLive(t *testing.T) {
	page := os.Getenv("AC_LIVE_URL")
	if page == "" {
		t.Skip("AC_LIVE_URL not set")
	}
	// test cwd is internal/cli → walk up to the repo root where .env lives
	var u, p string
	for _, path := range []string{".env", "../.env", "../../.env"} {
		if user, pass, err := readEnvFile(path); err == nil && user != "" {
			u, p = user, pass
			break
		}
	}
	if u == "" {
		t.Skip("no credentials found for AC_LIVE_URL test")
	}
	got := fetchTitle(context.Background(), page, "", nil, u, p)
	if got == "" {
		t.Fatal("empty title from live page (auth or parsing broken)")
	}
	if strings.Contains(got, "&#") || strings.EqualFold(got, "Adobe Connect Central Login") {
		t.Errorf("title not usable as a name: %q", got)
	}
	t.Logf("live title: %s", got)
}

func TestNewProgressLabels(t *testing.T) {
	// the printed stage name must come from the callback, not be hardcoded
	for _, stage := range []string{"download", "extract", "reconstruct"} {
		var buf bytes.Buffer
		p := newProgress(&buf)
		p(stage, 100, 200)
		if !strings.Contains(buf.String(), stage+" ") {
			t.Errorf("progress for stage %q printed %q", stage, buf.String())
		}
		if !strings.Contains(buf.String(), "elapsed]") {
			t.Errorf("progress for stage %q misses the elapsed-time suffix: %q", stage, buf.String())
		}
	}
}
