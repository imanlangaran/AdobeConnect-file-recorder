package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveFolderLive checks the real recording page + sco-info API.
// Skipped unless AC_LIVE_URL is set (e.g. AC_LIVE_URL=https://webinar5.um.ac.ir/p15rdeovw4ge/)
// because it needs network + credentials from ./.env.
func TestResolveFolderLive(t *testing.T) {
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
	id, name := resolveFolder(context.Background(), page, u, p)
	if id == "" {
		t.Fatal("empty folder id (auth or parsing broken)")
	}
	t.Logf("live folder: id=%s name=%q", id, name)
}

// TestEnsureGroupLive: creation + id-reuse against the real OUT_DIR layout.
// Skipped unless AC_LIVE_OUT is set (AC_LIVE_OUT=./recordings AC_LIVE_URL=…).
func TestEnsureGroupLive(t *testing.T) {
	out := os.Getenv("AC_LIVE_OUT")
	if out == "" {
		t.Skip("AC_LIVE_OUT not set")
	}
	g1 := ensureGroup(out, "999999", "Live Test Folder")
	if g1 != "999999_Live Test Folder" {
		t.Fatalf("created group = %q", g1)
	}
	if g2 := ensureGroup(out, "999999", "Renamed"); g2 != g1 {
		t.Fatalf("rename reused dir = %q, want %q", g2, g1)
	}
	// cleanup: this test's own fixture only
	_ = os.RemoveAll(filepath.Join(out, g1))
}

// TestEnsureGroup: first call creates "<id>_<name>" + folder.json; a rename on
// the server reuses the existing directory; unresolved ids land in _unfiled.
func TestEnsureGroup(t *testing.T) {
	out := t.TempDir()

	got := ensureGroup(out, "44063", "تصویر پردازی رقمی (گروه 200)")
	want := "44063_تصویر پردازی رقمی (گروه 200)"
	if got != want {
		t.Fatalf("ensureGroup = %q, want %q", got, want)
	}
	refPath := filepath.Join(out, got, "folder.json")
	b, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatalf("folder.json: %v", err)
	}
	var ref folderRef
	if err := json.Unmarshal(b, &ref); err != nil || ref.ID != "44063" || ref.Name == "" {
		t.Fatalf("folder.json = %q, want id 44063 + name", b)
	}

	// server-side rename: same id must reuse the directory, not fork a new one
	if got2 := ensureGroup(out, "44063", "A Totally New Name"); got2 != got {
		t.Fatalf("renamed folder reused dir = %q, want %q", got2, got)
	}
	// a path-unsafe name is sanitized but keeps its id prefix
	if g := ensureGroup(out, "7", "a/b:c"); strings.ContainsAny(g, `/\:`) {
		t.Fatalf("unsafe group name: %q", g)
	}
	// unresolved folder → shared _unfiled group
	if g := ensureGroup(out, "", "x"); g != unfiledGroup {
		t.Fatalf("unresolved group = %q, want %q", g, g)
	}
}

// TestResolveFolder: playback page → eventSco → sco-info → folder-id → name.
func TestResolveFolder(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rec/", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("BREEZESESSION"); err != nil || c.Value != "s1" {
			http.Redirect(w, r, "/login/", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<html><body><script>var eventSco = "223446";</script></body></html>`)
	})
	mux.HandleFunc("/api/xml", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "login" {
			http.SetCookie(w, &http.Cookie{Name: "BREEZESESSION", Value: "s1"})
			fmt.Fprint(w, `<results><status code="ok"/></results>`)
			return
		}
		if c, err := r.Cookie("BREEZESESSION"); err != nil || c.Value != "s1" {
			http.Error(w, "no-access", http.StatusForbidden)
			return
		}
		switch r.URL.Query().Get("sco-id") {
		case "223446": // the recording's sco → its parent folder
			fmt.Fprint(w, `<results><status code="ok"/><sco account-id="7" folder-id="44063" sco-id="223446" type="content"><name>x_0</name></sco></results>`)
		case "44063": // the folder → its human name
			fmt.Fprint(w, `<results><status code="ok"/><sco account-id="7" folder-id="43971" sco-id="44063" type="meeting"><name>&#1578;&#1586;&#1610;&#1585; &#1711;&#1585;&#1608;&#1607; 200</name></sco></results>`)
		default:
			fmt.Fprint(w, `<results><status code="no-access"/></results>`)
		}
	})
	mux.HandleFunc("/api/xml?action=login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "BREEZESESSION", Value: "s1"})
		fmt.Fprint(w, `<results><status code="ok"/></results>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id, name := resolveFolder(context.Background(), srv.URL+"/rec/", "u", "p")
	if id != "44063" {
		t.Fatalf("resolveFolder id = %q, want 44063", id)
	}
	// &#1578;&#1586;&#1610;&#1585; &#1711;&#1585;&#1608;&#1607; 200 → Arabic-form ي
	if name != "تزير گروه 200" {
		t.Fatalf("resolveFolder name = %q, want decoded folder name", name)
	}

	// unreachable/unauthorized → unresolved, caller falls back to _unfiled
	if id, name := resolveFolder(context.Background(), "http://127.0.0.1:1/rec/", "u", "p"); id != "" || name != "" {
		t.Fatalf("dead server: got %q/%q, want empty", id, name)
	}
}
