package queue

import (
	"errors"
	"fmt"
	"testing"
	"time"
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
	if a.ID == "" || a.Status != StatusQueued || existed {
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

	claimed, ok := s.Claim()
	if !ok {
		t.Fatal("Claim: nothing claimed")
	}
	if claimed.ID != a.ID || claimed.Status != StatusDownloading {
		t.Fatalf("claimed wrong task: %+v", claimed)
	}

	if err := s.SetStatus(b.ID, StatusProcessing, ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := s.SetDone(a.ID, `{"ok":true}`); err != nil {
		t.Fatalf("SetDone: %v", err)
	}

	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusDone || got.ReportJSON != `{"ok":true}` {
		t.Fatalf("bad done task: %+v", got)
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: want ErrNotFound, got %v", err)
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
	if counts[StatusDone] != 1 || counts[StatusProcessing] != 1 {
		t.Fatalf("bad counts: %+v", counts)
	}

	// Failed task may be re-enqueued for the same URL (no active row).
	if err := s.SetStatus(b.ID, StatusFailed, "boom"); err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}
	if _, ok := s.Claim(); ok {
		t.Fatal("Claim after all non-queued: got task")
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
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if n, err := s.RequeueActive(); err != nil || n != 0 {
		t.Fatalf("RequeueActive empty = %d, %v", n, err)
	}
	a, _, _ := s.Enqueue("https://example.test/r1", "r1")
	b, _, _ := s.Enqueue("https://example.test/r2", "r2")
	claimed, ok := s.Claim() // a -> downloading, attempt 1
	if !ok || claimed.ID != a.ID {
		t.Fatalf("Claim: %+v %v", claimed, ok)
	}
	if err := s.SetStatus(b.ID, StatusProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RequeueActive(); err != nil || n != 2 {
		t.Fatalf("RequeueActive = %d, %v; want 2", n, err)
	}
	for _, id := range []string{a.ID, b.ID} {
		got, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusQueued {
			t.Fatalf("task %s status = %s, want queued", id, got.Status)
		}
		if got.LastError == "" {
			t.Fatalf("task %s lost the restart note", id)
		}
	}
	reclaimed, ok := s.Claim()
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
	if got, err := s.Get(active.ID); err != nil || got.Status != StatusQueued {
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
	if _, err := s.Get(task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task should be deleted, got err=%v", err)
	}
	if err := s.Delete("non-existent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete non-existent want ErrNotFound, got %v", err)
	}
}
