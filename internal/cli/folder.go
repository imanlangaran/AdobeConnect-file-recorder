// Parent-folder grouping: all recordings of one class (meeting) live in
// recordings/<folder-id>_<name>/<recording title>/. The folder id is the
// machine-readable key (renames reuse the directory); the human name sits in
// the dir name and in folder.json.
package cli

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/download"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/engine"
)

// unfiledGroup holds recordings whose parent folder could not be resolved.
const unfiledGroup = "_unfiled"

// folderRef is folder.json: the id→name record written once per folder dir.
type folderRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var (
	// eventScoRe reads the recording's sco-id from its playback page.
	eventScoRe = regexp.MustCompile(`var eventSco = "(\d+)"`)
	// folderIDRe reads the owning folder from a sco-info response.
	folderIDRe = regexp.MustCompile(`folder-id="(\d+)"`)
	// scoNameRe reads the display name from a sco-info response.
	scoNameRe = regexp.MustCompile(`(?is)<name>(.*?)</name>`)
)

// resolveFolder maps a recording URL to its parent folder (the meeting SCO
// owning every recording of one class): page → eventSco → sco-info →
// folder-id → sco-info → name. Best-effort: ("", "") when the server is
// unreachable or denies access; callers fall back to the _unfiled group.
func resolveFolder(ctx context.Context, pageURL, user, pass string) (id, name string) {
	client := &http.Client{Timeout: 15 * time.Second}
	var cookies []*http.Cookie
	if user != "" {
		cs, err := download.Login(ctx, client, originOf(pageURL), user, pass)
		if err != nil {
			return "", ""
		}
		cookies = cs
	}
	page := fetchBody(ctx, client, originOf(pageURL), pageURL, cookies)
	if m := eventScoRe.FindSubmatch(page); m != nil {
		info := fetchBody(ctx, client, originOf(pageURL),
			originOf(pageURL)+"/api/xml?action=sco-info&sco-id="+string(m[1]), cookies)
		if f := folderIDRe.FindSubmatch(info); f != nil {
			id = string(f[1])
		}
	}
	if id == "" {
		return "", ""
	}
	folder := fetchBody(ctx, client, originOf(pageURL),
		originOf(pageURL)+"/api/xml?action=sco-info&sco-id="+id, cookies)
	if n := scoNameRe.FindSubmatch(folder); n != nil {
		name = strings.Join(strings.Fields(html.UnescapeString(string(n[1]))), " ")
	}
	return id, name
}

// fetchBody GETs url with cookies and returns up to 1 MiB, or nil on failure.
func fetchBody(ctx context.Context, client *http.Client, origin, url string, cookies []*http.Cookie) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36")
	if origin != "" {
		req.Header.Set("Referer", origin+"/")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	return b
}

// ensureGroup returns the output subdirectory for a recording folder, creating
// it when new: an existing folder.json with the same id wins (a renamed class
// reuses its directory), otherwise "<id>_<name>" is created. Unresolved
// folders land in the shared _unfiled group.
func ensureGroup(outDir, id, name string) string {
	if id == "" {
		return unfiledGroup
	}
	if matches, err := filepath.Glob(filepath.Join(outDir, "*", "folder.json")); err == nil {
		for _, m := range matches {
			var ref folderRef
			if b, err := os.ReadFile(m); err == nil &&
				json.Unmarshal(b, &ref) == nil && ref.ID == id {
				return filepath.Base(filepath.Dir(m))
			}
		}
	}
	group := engine.Sanitize(id)
	if name != "" {
		group = engine.Sanitize(id + "_" + name)
	}
	if err := os.MkdirAll(filepath.Join(outDir, group), 0o755); err != nil {
		return unfiledGroup
	}
	if b, err := json.MarshalIndent(folderRef{ID: id, Name: name}, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(outDir, group, "folder.json"), b, 0o644)
	}
	return group
}
