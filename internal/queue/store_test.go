package queue

import (
	"errors"
	"fmt"
	"testing"
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
