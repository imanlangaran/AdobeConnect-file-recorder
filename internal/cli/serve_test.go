package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/config"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/domain"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/repository/filestore"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/server"
)

// TestFailTaskRetryBudget: attempts within budget requeue, exhausted ones fail.
func TestFailTaskRetryBudget(t *testing.T) {
	cfg := config.Config{RetryCount: 2}
	newTask := func() (domain.Repository, domain.Task) {
		s, err := filestore.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		task, existed, err := s.Enqueue("https://example.test/r1", "r1")
		if err != nil || existed {
			t.Fatalf("Enqueue: %+v %v %v", task, existed, err)
		}
		if _, ok := s.Claim(); !ok { // attempts=1
			t.Fatal("Claim: nothing claimed")
		}
		task, err = s.Get(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s, task
	}

	s, task := newTask() // attempts=1 <= 2 → requeue
	failTask(context.Background(), s, cfg, task, errors.New("boom"))
	if got, _ := s.Get(task.ID); got.Status != domain.StatusQueued {
		t.Fatalf("within budget: status=%s, want queued", got.Status)
	}

	s, task = newTask()
	task.Attempts = 3 // > 2 → terminal
	failTask(context.Background(), s, cfg, task, errors.New("boom"))
	if got, _ := s.Get(task.ID); got.Status != domain.StatusFailed || got.LastError != "boom" {
		t.Fatalf("exhausted: %+v, want failed/boom", got)
	}
}

// TestFailTaskShutdownRequeues: a cancelled context requeues immediately even
// past the budget (a fresh process retries it).
func TestFailTaskShutdownRequeues(t *testing.T) {
	s, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task, _, _ := s.Enqueue("https://example.test/r1", "r1")
	if _, ok := s.Claim(); !ok {
		t.Fatal("Claim: nothing claimed")
	}
	task, _ = s.Get(task.ID)
	task.Attempts = 99
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failTask(ctx, s, config.Config{RetryCount: 0}, task, errors.New("boom"))
	if got, _ := s.Get(task.ID); got.Status != domain.StatusQueued {
		t.Fatalf("shutdown: status=%s, want queued", got.Status)
	}
}

// TestQueueAdapterDedupeFlag: duplicate URLs report existed=true (200),
// new ones existed=false (202).
func TestQueueAdapterDedupeFlag(t *testing.T) {
	s, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := queueAdapter{q: s}
	if _, existed, err := a.Enqueue("https://example.test/r1", "r1"); err != nil || existed {
		t.Fatalf("first enqueue: existed=%v err=%v", existed, err)
	}
	got, existed, err := a.Enqueue("https://example.test/r1", "r1")
	if err != nil || !existed {
		t.Fatalf("dupe enqueue: existed=%v err=%v", existed, err)
	}
	if _, ok := interface{}(got).(server.Task); !ok {
		t.Fatalf("adapter must return server.Task, got %T", got)
	}
}

// TestEnqueueTaskIsBornGrouped: a claimed task must already carry its group —
// persisting it ungrouped lets a worker claim it before folder resolution
// finishes, and the recording then lands flat instead of in its class folder.
func TestEnqueueTaskIsBornGrouped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/xml" && r.URL.Query().Get("action") == "login":
			http.SetCookie(w, &http.Cookie{Name: "BREEZESESSION", Value: "s1"})
			fmt.Fprint(w, `<results><status code="ok"/></results>`)
		case r.URL.Path == "/api/xml" && r.URL.Query().Get("sco-id") == "223446":
			fmt.Fprint(w, `<results><status code="ok"/><sco folder-id="44063" sco-id="223446"><name>x_0</name></sco></results>`)
		case r.URL.Path == "/api/xml" && r.URL.Query().Get("sco-id") == "44063":
			fmt.Fprint(w, `<results><status code="ok"/><sco sco-id="44063"><name>Class 200</name></sco></results>`)
		case r.URL.Path == "/rec/":
			fmt.Fprint(w, `<html><script>var eventSco = "223446";</script></html>`)
		default:
			http.Error(w, "no-access", http.StatusForbidden)
		}
	}))
	defer srv.Close()

	out := t.TempDir()
	s, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := queueAdapter{q: s, cfg: config.Config{ACUsername: "u", ACPassword: "p", OutDir: out}}
	if _, existed, err := a.Enqueue(srv.URL+"/rec/", "r1"); err != nil || existed {
		t.Fatalf("enqueue: existed=%v err=%v", existed, err)
	}
	claimed, ok := s.Claim()
	if !ok {
		t.Fatal("no task claimed")
	}
	if claimed.Group != "44063_Class 200" {
		t.Fatalf("claimed group = %q, want %q (must be set before claim)", claimed.Group, "44063_Class 200")
	}
	if _, err := os.Stat(filepath.Join(out, claimed.Group, "folder.json")); err != nil {
		t.Fatalf("folder.json missing: %v", err)
	}
}

// TestAdapterRetryAndStop: the bridge's retry modes (full/reconstruct/resume)
// and stop-when-idle behaviour.
func TestAdapterRetryAndStop(t *testing.T) {
	dir := t.TempDir()
	s, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := queueAdapter{q: s, cfg: config.Config{DataDir: dir}, rs: newRuns()}
	cache := func(id string) string { return filepath.Join(dir, "cache", id+".zip") }

	task, _, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	// Stop: nothing running -> false, task untouched.
	if a.Stop(task.ID) {
		t.Fatal("Stop on idle task must report false")
	}
	if got, _ := s.Get(task.ID); got.Status != domain.StatusQueued {
		t.Fatalf("idle stop mutated task: %+v", got)
	}
	// Retry queued -> server.ErrBadState; unknown id -> server.ErrNotFound.
	if err := a.Retry(task.ID, "full"); !errors.Is(err, server.ErrBadState) {
		t.Fatalf("retry queued: %v, want ErrBadState", err)
	}
	if err := a.Retry("missing", "full"); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("retry missing: %v, want ErrNotFound", err)
	}

	// Reconstruct-only without a cached archive must be rejected.
	if err := s.SetStatus(task.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := a.Retry(task.ID, "reconstruct"); err == nil ||
		!strings.Contains(err.Error(), "no cached archive") {
		t.Fatalf("reconstruct w/o cache: %v", err)
	}

	// Full retry drops the stale cache, requeues with a fresh budget.
	if err := os.MkdirAll(filepath.Dir(cache(task.ID)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache(task.ID), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Retry(task.ID, "full"); err != nil {
		t.Fatalf("full retry: %v", err)
	}
	if _, err := os.Stat(cache(task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("full retry must drop the cache, stat: %v", err)
	}
	if got, _ := s.Get(task.ID); got.Status != domain.StatusQueued || got.SkipDownload {
		t.Fatalf("after full retry: %+v", got)
	}

	// Valid cache -> reconstruct-only retry sets SkipDownload.
	if err := os.WriteFile(cache(task.ID), []byte("PK\x03\x04rest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(task.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := a.Retry(task.ID, "reconstruct"); err != nil {
		t.Fatalf("reconstruct retry: %v", err)
	}
	if got, _ := s.Get(task.ID); got.Status != domain.StatusQueued || !got.SkipDownload {
		t.Fatalf("after reconstruct retry: %+v", got)
	}

	// Resume: only a paused task, via the running-task stop path.
	if _, ok := s.Claim(); !ok {
		t.Fatal("Claim: nothing to claim")
	}
	if err := s.Pause(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.Retry(task.ID, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := s.Get(task.ID); got.Status != domain.StatusQueued || !got.SkipDownload {
		t.Fatalf("after resume: %+v (must keep the reconstruct-only flag)", got)
	}

	// Stop with a registered run: cancels and parks the task in paused.
	if _, ok := s.Claim(); !ok { // a registered run implies an active task
		t.Fatal("Claim: nothing to claim")
	}
	rh := a.rs.add(task.ID, func() {})
	defer a.rs.remove(task.ID)
	if !a.Stop(task.ID) {
		t.Fatal("Stop on registered run must report true")
	}
	if !rh.stopped.Load() {
		t.Fatal("Stop must mark the run stopped")
	}
	if got, _ := s.Get(task.ID); got.Status != domain.StatusPaused {
		t.Fatalf("after stop: %+v, want paused", got)
	}
}

// TestToServerTaskKeepsFolder: the HTTP layer must not drop the parent-folder
// fields — the React table groups rows by them, and /api/tasks must carry
// the folderID/folderName/group JSON keys.
func TestToServerTaskKeepsFolder(t *testing.T) {
	qt := domain.Task{ID: "t1", URL: "https://example.test/r1", RecordingID: "r1",
		FolderID: "44063", FolderName: "تصوير", Group: "44063_تصوير",
		Status: domain.StatusQueued}
	got := toServerTask(qt)
	if got.FolderID != "44063" || got.FolderName != "تصوير" || got.Group != "44063_تصوير" {
		t.Fatalf("toServerTask dropped folder info: %+v", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"folderID":"44063"`, `"folderName":"تصوير"`, `"group":"44063_تصوير"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("task JSON missing %s: %s", want, b)
		}
	}
}

func TestFormatStage(t *testing.T) {
	tests := []struct {
		stage string
		done  int64
		total int64
		want  string
	}{
		{"download", 50 * (1 << 20), 100 * (1 << 20), "download 50% (50.0/100.0 MB)"},
		{"download", 10 * (1 << 20), 0, "download 10.0 MB"},
		{"reconstruct", 30000, 60000, "reconstruct 50% (0:30/1:00)"},
		{"reconstruct", 45000, 0, "reconstruct 0:45"},
	}
	for _, tt := range tests {
		got := formatStage(tt.stage, tt.done, tt.total)
		if got != tt.want {
			t.Errorf("formatStage(%q, %d, %d) = %q, want %q", tt.stage, tt.done, tt.total, got, tt.want)
		}
	}
}
