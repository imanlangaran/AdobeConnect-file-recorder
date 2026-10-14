package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/config"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/queue"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/server"
)

// TestFailTaskRetryBudget: attempts within budget requeue, exhausted ones fail.
func TestFailTaskRetryBudget(t *testing.T) {
	cfg := config.Config{RetryCount: 2}
	newTask := func() (*queue.Store, queue.Task) {
		s, err := queue.Open(t.TempDir())
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
	if got, _ := s.Get(task.ID); got.Status != queue.StatusQueued {
		t.Fatalf("within budget: status=%s, want queued", got.Status)
	}

	s, task = newTask()
	task.Attempts = 3 // > 2 → terminal
	failTask(context.Background(), s, cfg, task, errors.New("boom"))
	if got, _ := s.Get(task.ID); got.Status != queue.StatusFailed || got.LastError != "boom" {
		t.Fatalf("exhausted: %+v, want failed/boom", got)
	}
}

// TestFailTaskShutdownRequeues: a cancelled context requeues immediately even
// past the budget (a fresh process retries it).
func TestFailTaskShutdownRequeues(t *testing.T) {
	s, err := queue.Open(t.TempDir())
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
	if got, _ := s.Get(task.ID); got.Status != queue.StatusQueued {
		t.Fatalf("shutdown: status=%s, want queued", got.Status)
	}
}

// TestQueueAdapterDedupeFlag: duplicate URLs report existed=true (200),
// new ones existed=false (202).
func TestQueueAdapterDedupeFlag(t *testing.T) {
	s, err := queue.Open(t.TempDir())
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
