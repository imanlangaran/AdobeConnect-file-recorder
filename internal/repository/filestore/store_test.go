package filestore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/domain"
)

func TestEnqueueDedupeClaimSetDoneList(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	a, existed, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if a.ID == "" || a.Status != domain.StatusQueued || existed {
		t.Fatalf("bad task: %+v existed=%v", a, existed)
	}

	dup, dupHit, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatalf("Enqueue dup: %v", err)
	}
	if !dupHit {
		t.Fatal("dedupe hit must report existed=true")
	}
	if dup.ID != a.ID {
		t.Fatalf("dedupe failed: %q vs %q", dup.ID, a.ID)
	}

	b, _, err := s.Enqueue("https://example.test/r2", "r2")
	if err != nil {
		t.Fatalf("Enqueue b: %v", err)
	}

	claimed, ok := s.ClaimDownload()
	if !ok {
		t.Fatal("ClaimDownload: nothing claimed")
	}
	if claimed.ID != a.ID || claimed.Status != domain.StatusDownloading {
		t.Fatalf("claimed wrong task: %+v", claimed)
	}

	if err := s.SetStatus(a.ID, domain.StatusDownloaded, ""); err != nil {
		t.Fatal(err)
	}
	reclaimed, ok := s.ClaimReconstruct()
	if !ok || reclaimed.ID != a.ID || reclaimed.Status != domain.StatusProcessing {
		t.Fatalf("ClaimReconstruct: %+v %v", reclaimed, ok)
	}

	if err := s.SetStatus(b.ID, domain.StatusProcessing, ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := s.SetDone(a.ID, `{"ok":true}`); err != nil {
		t.Fatalf("SetDone: %v", err)
	}

	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.StatusDone || got.ReportJSON != `{"ok":true}` {
		t.Fatalf("bad done task: %+v", got)
	}
	if _, err := s.Get("missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: want domain.ErrNotFound, got %v", err)
	}

	list, err := s.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("bad list order: %+v", list)
	}
	if lim, err := s.List(1); err != nil || len(lim) != 1 {
		t.Fatalf("List(1): %+v %v", lim, err)
	}

	counts, err := s.Counts()
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts[domain.StatusDone] != 1 || counts[domain.StatusProcessing] != 1 {
		t.Fatalf("bad counts: %+v", counts)
	}

	// Failed task may be re-enqueued for the same URL (no active row).
	if err := s.SetStatus(b.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}
	if _, ok := s.ClaimDownload(); ok {
		t.Fatal("ClaimDownload after all non-queued: got task")
	}
	if _, ok := s.ClaimReconstruct(); ok {
		t.Fatal("ClaimReconstruct after all non-downloaded: got task")
	}
	c, _, err := s.Enqueue("https://example.test/r2", "r2")
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if c.ID == b.ID {
		t.Fatal("re-enqueue after failure should create a new task")
	}
}

func TestRequeueActive(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if n, err := s.RequeueActive(); err != nil || n != 0 {
		t.Fatalf("RequeueActive empty = %d, %v", n, err)
	}
	a, _, _ := s.Enqueue("https://example.test/r1", "r1")
	b, _, _ := s.Enqueue("https://example.test/r2", "r2")
	c, _, _ := s.Enqueue("https://example.test/r3", "r3")
	d, _, _ := s.Enqueue("https://example.test/r4", "r4")

	claimed, ok := s.ClaimDownload() // a -> downloading, attempt 1
	if !ok || claimed.ID != a.ID {
		t.Fatalf("ClaimDownload: %+v %v", claimed, ok)
	}

	// b: downloaded -> processing with cached zip file
	if err := s.SetStatus(b.ID, domain.StatusDownloaded, ""); err != nil {
		t.Fatal(err)
	}
	if claimedB, ok := s.ClaimReconstruct(); !ok || claimedB.ID != b.ID {
		t.Fatalf("ClaimReconstruct b: %+v %v", claimedB, ok)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache", b.ID+".zip"), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	// c: downloaded -> processing without cached zip file
	if err := s.SetStatus(c.ID, domain.StatusDownloaded, ""); err != nil {
		t.Fatal(err)
	}
	if claimedC, ok := s.ClaimReconstruct(); !ok || claimedC.ID != c.ID {
		t.Fatalf("ClaimReconstruct c: %+v %v", claimedC, ok)
	}

	// d: downloaded, idle -> must remain downloaded
	if err := s.SetStatus(d.ID, domain.StatusDownloaded, ""); err != nil {
		t.Fatal(err)
	}

	if n, err := s.RequeueActive(); err != nil || n != 3 {
		t.Fatalf("RequeueActive = %d, %v; want 3", n, err)
	}

	gotA, _ := s.Get(a.ID)
	if gotA.Status != domain.StatusQueued || gotA.LastError == "" {
		t.Fatalf("task a = %+v, want queued with restart note", gotA)
	}
	gotB, _ := s.Get(b.ID)
	if gotB.Status != domain.StatusDownloaded || gotB.LastError == "" {
		t.Fatalf("task b = %+v, want downloaded with restart note", gotB)
	}
	gotC, _ := s.Get(c.ID)
	if gotC.Status != domain.StatusQueued || gotC.LastError == "" {
		t.Fatalf("task c = %+v, want queued with restart note", gotC)
	}
	gotD, _ := s.Get(d.ID)
	if gotD.Status != domain.StatusDownloaded {
		t.Fatalf("task d = %+v, want downloaded unchanged", gotD)
	}

	reclaimed, ok := s.ClaimDownload()
	if !ok || reclaimed.ID != a.ID || reclaimed.Attempts != 2 {
		t.Fatalf("re-claim: %+v %v (attempts must resume, not reset)", reclaimed, ok)
	}
}

func TestPruneTerminal(t *testing.T) {
	orig := maxRetainedTasks
	maxRetainedTasks = 3
	defer func() { maxRetainedTasks = orig }()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	active, _, err := s.Enqueue("https://example.test/active", "active")
	if err != nil {
		t.Fatal(err)
	}
	// Seed 5 terminal rows (3 will be kept, 2 pruned)
	for i := 0; i < 5; i++ {
		task, _, err := s.Enqueue(fmt.Sprintf("https://example.test/seed-%d", i), fmt.Sprintf("seed-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetDone(task.ID, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneTerminal()
	if err != nil || n != 2 {
		t.Fatalf("PruneTerminal = %d, %v; want 2", n, err)
	}
	list, err := s.List(0)
	if err != nil {
		t.Fatal(err)
	}
	// 3 terminal kept + 1 active = 4
	if len(list) != 4 {
		t.Fatalf("after prune: %d tasks, want 4", len(list))
	}
	if got, err := s.Get(active.ID); err != nil || got.Status != domain.StatusQueued {
		t.Fatalf("prune must keep active rows: %+v %v", got, err)
	}
}

func TestSetStage(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	task, _, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStage(task.ID, "download 50%"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != "download 50%" {
		t.Fatalf("Stage = %q, want 'download 50%%'", got.Stage)
	}
	// SetDone clears stage
	if err := s.SetDone(task.ID, "{}"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(task.ID)
	if got.Stage != "" {
		t.Fatalf("SetDone must clear stage, got %q", got.Stage)
	}
}

// TestSetGroup: a task born ungrouped (registration-time resolution failed)
// is healed by the worker — SetGroup must persist all four fields.
func TestSetGroup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	task, _, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(task.ID); got.Group != "" || got.FolderID != "" {
		t.Fatalf("fresh task group = %q/%q, want empty", got.FolderID, got.Group)
	}
	if err := s.SetGroup(task.ID, "44063", "تصوير", "44063_تصوير"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FolderID != "44063" || got.FolderName != "تصوير" || got.Group != "44063_تصوير" {
		t.Fatalf("after SetGroup = %q/%q/%q", got.FolderID, got.FolderName, got.Group)
	}
}

func TestRetryResumePause(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	task, _, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok := s.ClaimDownload()
	if !ok || claimed.Attempts != 1 {
		t.Fatalf("claim: %+v, want attempts=1", claimed)
	}

	// Pause: only running tasks, only existing ones.
	if err := s.Pause("missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Pause missing: %v", err)
	}
	if err := s.SetStatus(task.ID, domain.StatusQueued, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(task.ID); !errors.Is(err, domain.ErrBadState) {
		t.Fatalf("Pause queued must be domain.ErrBadState, got %v", err)
	}
	if _, ok := s.ClaimDownload(); !ok { // re-claim for the pause path
		t.Fatal("ClaimDownload: nothing to claim")
	}
	if err := s.Pause(task.ID); err != nil {
		t.Fatalf("Pause running: %v", err)
	}
	got, _ := s.Get(task.ID)
	if got.Status != domain.StatusPaused || got.Stage != "" {
		t.Fatalf("after Pause: %+v", got)
	}
	if _, ok := s.ClaimDownload(); ok {
		t.Fatal("ClaimDownload must skip paused tasks")
	}

	// Resume: only from paused, attempts preserved.
	if err := s.Resume(task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, _ = s.Get(task.ID)
	if got.Status != domain.StatusQueued || got.Attempts != claimed.Attempts+1 { // claimed twice
		t.Fatalf("after Resume: %+v", got)
	}
	if err := s.Resume(task.ID); !errors.Is(err, domain.ErrBadState) { // now queued
		t.Fatalf("Resume queued must be domain.ErrBadState, got %v", err)
	}

	// Retry: only from failed/paused/done; resets attempts, sets mode.
	if err := s.Retry(task.ID, true); !errors.Is(err, domain.ErrBadState) {
		t.Fatalf("Retry queued must be domain.ErrBadState, got %v", err)
	}
	if err := s.SetStatus(task.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(task.ID, true); err != nil {
		t.Fatalf("Retry failed task: %v", err)
	}
	got, _ = s.Get(task.ID)
	if got.Status != domain.StatusDownloaded || got.Attempts != 0 || !got.SkipDownload ||
		got.LastError != "" || got.Stage != "" {
		t.Fatalf("after reconstruct Retry: %+v", got)
	}
	if err := s.SetStatus(task.ID, domain.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(task.ID, false); err != nil {
		t.Fatalf("Retry full task: %v", err)
	}
	got, _ = s.Get(task.ID)
	if got.Status != domain.StatusQueued || got.Attempts != 0 || got.SkipDownload {
		t.Fatalf("after full Retry: %+v", got)
	}

	// Counts includes paused.
	if _, ok := s.ClaimDownload(); !ok {
		t.Fatal("ClaimDownload: nothing to claim")
	}
	if err := s.Pause(task.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	counts, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[domain.StatusPaused] != 1 {
		t.Fatalf("counts missing paused: %+v", counts)
	}
}

func TestDeleteAndSubscribe(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	updated := make(chan struct{}, 5)
	s.Subscribe(func() {
		updated <- struct{}{}
	})

	task, _, err := s.Enqueue("https://example.test/r1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("expected subscribe notification on enqueue")
	}

	if err := s.Delete(task.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("task should be deleted, got err=%v", err)
	}
	if err := s.Delete("non-existent"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete non-existent want domain.ErrNotFound, got %v", err)
	}
}
