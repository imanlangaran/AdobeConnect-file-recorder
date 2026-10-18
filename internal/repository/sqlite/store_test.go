package sqlite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/domain"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEnqueueDedupeClaimDone(t *testing.T) {
	s := open(t)

	a, existed, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil || existed || a.Status != domain.StatusQueued {
		t.Fatalf("Enqueue: %+v existed=%v err=%v", a, existed, err)
	}
	dup, hit, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil || !hit || dup.ID != a.ID {
		t.Fatalf("dedupe: %+v hit=%v err=%v", dup, hit, err)
	}
	b, _, err := s.Enqueue("https://example.test/r2", "r2")
	if err != nil {
		t.Fatal(err)
	}

	claimed, ok := s.Claim()
	if !ok || claimed.ID != a.ID || claimed.Status != domain.StatusDownloading || claimed.Attempts != 1 {
		t.Fatalf("Claim: %+v ok=%v (oldest first, attempts incremented)", claimed, ok)
	}
	if err := s.SetStatus(b.ID, domain.StatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDone(a.ID, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(a.ID)
	if err != nil || got.Status != domain.StatusDone || got.ReportJSON != `{"ok":true}` || got.Stage != "" {
		t.Fatalf("Get done: %+v err=%v", got, err)
	}
	if _, err := s.Get("missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
	}

	// worked fields survive a round-trip
	if err := s.SetGroup(b.ID, "44063", "Class", "44063_Class"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStage(b.ID, "download 50%"); err != nil {
		t.Fatal(err)
	}
	if b2, _ := s.Get(b.ID); b2.Group != "44063_Class" || b2.FolderName != "Class" || b2.Stage != "download 50%" {
		t.Fatalf("round-trip lost fields: %+v", b2)
	}

	list, err := s.List(0)
	if err != nil || len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("List: %+v err=%v (newest first)", list, err)
	}
	counts, err := s.Counts()
	if err != nil || counts[domain.StatusDone] != 1 || counts[domain.StatusProcessing] != 1 {
		t.Fatalf("Counts: %+v err=%v", counts, err)
	}

	// a failed URL can be re-enqueued as a new task
	if err := s.SetStatus(b.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	c, _, err := s.Enqueue("https://example.test/r2", "r2")
	if err != nil || c.ID == b.ID {
		t.Fatalf("re-enqueue after failure: %+v err=%v", c, err)
	}
}

func TestStatesAndErrors(t *testing.T) {
	s := open(t)
	task, _, _ := s.Enqueue("https://example.test/r1", "r1")

	if err := s.Pause("missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Pause missing: %v", err)
	}
	if err := s.Pause(task.ID); !errors.Is(err, domain.ErrBadState) {
		t.Fatalf("Pause queued: want ErrBadState, got %v", err)
	}
	if _, ok := s.Claim(); !ok {
		t.Fatal("Claim: nothing")
	}
	if err := s.Pause(task.ID); err != nil {
		t.Fatalf("Pause running: %v", err)
	}
	if got, _ := s.Get(task.ID); got.Status != domain.StatusPaused {
		t.Fatalf("after Pause: %+v", got)
	}
	if err := s.Resume(task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := s.Resume(task.ID); !errors.Is(err, domain.ErrBadState) {
		t.Fatalf("Resume queued: want ErrBadState, got %v", err)
	}
	if err := s.Retry(task.ID, true); !errors.Is(err, domain.ErrBadState) {
		t.Fatalf("Retry queued: want ErrBadState, got %v", err)
	}
	if err := s.SetStatus(task.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(task.ID, true); err != nil {
		t.Fatalf("Retry failed: %v", err)
	}
	got, _ := s.Get(task.ID)
	if got.Status != domain.StatusQueued || got.Attempts != 0 || !got.SkipDownload || got.LastError != "" {
		t.Fatalf("after Retry: %+v", got)
	}
}

func TestPruneTerminal(t *testing.T) {
	orig := maxRetainedTasks
	maxRetainedTasks = 3
	defer func() { maxRetainedTasks = orig }()

	s := open(t)
	active, _, _ := s.Enqueue("https://example.test/active", "active")
	for i := 0; i < 5; i++ {
		task, _, _ := s.Enqueue(fmt.Sprintf("https://example.test/seed-%d", i), "seed")
		if err := s.SetDone(task.ID, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneTerminal()
	if err != nil || n != 2 {
		t.Fatalf("PruneTerminal = %d, %v; want 2", n, err)
	}
	list, _ := s.List(0)
	if len(list) != 4 { // 3 terminal + 1 active
		t.Fatalf("after prune: %d tasks, want 4", len(list))
	}
	if got, err := s.Get(active.ID); err != nil || got.Status != domain.StatusQueued {
		t.Fatalf("prune must keep active rows: %+v err=%v", got, err)
	}
}

// TestMigrateLegacyJSON checks the file -> sqlite move: tasks are imported and
// queue.json is renamed aside (never deleted).
func TestMigrateLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte(`{"tasks":[
		{"id":"t1","url":"https://example.test/a","recording_id":"a","status":"done","attempts":2,
		 "report_json":"{}","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:05:05Z"}
	]}`)
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	got, err := s.Get("t1")
	if err != nil || got.Status != domain.StatusDone || got.Attempts != 2 || got.CreatedAt.IsZero() {
		t.Fatalf("migrated task: %+v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.json.migrated")); err != nil {
		t.Fatalf("legacy file must be kept as .migrated: %v", err)
	}

	// reopen: no double import
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	list, _ := s2.List(0)
	if len(list) != 1 {
		t.Fatalf("reopen imported twice: %d tasks", len(list))
	}
}

// A failed migration must not strand queue.json behind an empty queue.db: the
// next start retries and imports once the file is readable.
func TestMigrateRetriesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "queue.json")
	if err := os.WriteFile(legacy, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("corrupt queue.json must fail Open")
	}
	if err := os.WriteFile(legacy, []byte(`{"tasks":[
		{"id":"t1","url":"https://example.test/a","recording_id":"a","status":"queued",
		 "created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}
	]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("retry Open: %v", err)
	}
	defer s.Close()
	if got, err := s.Get("t1"); err != nil || got.Status != domain.StatusQueued {
		t.Fatalf("task not imported on retry: %+v err=%v", got, err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("legacy file must be renamed on success: %v", err)
	}
}

// A '#' or '?' in DATA_DIR must not be read as a URI fragment/query: queue.db
// lives inside the requested directory.
func TestOpenEscapesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a#b")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if _, _, err := s.Enqueue("https://example.test/r1", "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.db")); err != nil {
		t.Fatalf("queue.db must live inside the requested dir: %v", err)
	}
}
