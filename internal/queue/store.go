// Package queue provides a persistent task queue for recording jobs.
//
// ponytail: JSON-file store holds up to ~10k tasks comfortably; migrate to
// SQLite (modernc.org/sqlite) if the queue grows large or multi-process
// atomic claim races start to matter.
package queue

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Status is the lifecycle state of a Task.
type Status string

const (
	StatusQueued      Status = "queued"
	StatusDownloading Status = "downloading"
	StatusProcessing  Status = "processing"
	StatusDone        Status = "done"
	StatusFailed      Status = "failed"
	StatusPaused      Status = "paused" // stopped by the user; resume requeues it
)

// Task is a single queued recording job.
type Task struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	RecordingID  string    `json:"recording_id"`
	FolderID     string    `json:"folder_id,omitempty"`   // parent meeting SCO id (machine key)
	FolderName   string    `json:"folder_name,omitempty"` // human name of that folder
	Group        string    `json:"group,omitempty"`       // output subdir under OUT_DIR
	Status       Status    `json:"status"`
	SkipDownload bool      `json:"skip_download,omitempty"` // reconstruct-only retry: reuse the cached archive
	Stage        string    `json:"stage,omitempty"`
	Attempts     int       `json:"attempts"`
	LastError    string    `json:"last_error,omitempty"`
	ReportJSON   string    `json:"report_json,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ErrNotFound is returned when no task with the given ID exists.
var ErrNotFound = errors.New("queue: task not found")

// ErrBadState is returned by Retry/Resume/Pause for a status transition that
// is not allowed (e.g. pausing a queued task).
var ErrBadState = errors.New("queue: invalid task state")

// Store is a mutex-guarded, file-backed task queue. Every mutation is
// persisted atomically (write temp file + rename) so a crash never leaves a
// half-written queue behind. Safe for concurrent use within one process.
type Store struct {
	mu        sync.Mutex
	path      string
	seq       atomic.Uint64
	listeners []func()
}

type diskState struct {
	Tasks []Task `json:"tasks"`
}

// Open creates dataDir if needed and opens (or initialises) queue.json inside it.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dataDir, "queue.json")}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		if err := s.saveLocked(nil); err != nil {
			return nil, err
		}
		return s, nil
	}
	if _, err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Enqueue adds a task, or returns the existing active task for the same URL
// (dedupe: a URL already queued, downloading, or processing is not re-added).
// existed reports whether the returned task predates this call, so callers
// can answer 200 (duplicate) vs 202 (new) without a separate scan.
func (s *Store) Enqueue(url, recordingID string) (Task, bool, error) {
	return s.EnqueueGrouped(url, recordingID, "", "", "")
}

// EnqueueGrouped registers a task already resolved to its parent folder. The
// group must be set here, not after: a task persisted ungrouped can be claimed
// by a worker before a later SetGroup and would output to the wrong directory.
func (s *Store) EnqueueGrouped(url, recordingID, folderID, folderName, group string) (t Task, existed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return Task{}, false, err
	}
	for _, t := range tasks {
		if t.URL == url && (t.Status == StatusQueued || t.Status == StatusDownloading || t.Status == StatusProcessing) {
			return t, true, nil
		}
	}
	now := time.Now().UTC()
	t = Task{ID: s.newID(now), URL: url, RecordingID: recordingID,
		FolderID: folderID, FolderName: folderName, Group: group,
		Status: StatusQueued, CreatedAt: now, UpdatedAt: now}
	tasks = append(tasks, t)
	if err := s.saveLocked(tasks); err != nil {
		return Task{}, false, err
	}
	return t, false, nil
}

// Claim picks the oldest queued task and marks it downloading.
// The second return value is false when no queued task exists.
func (s *Store) Claim() (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return Task{}, false
	}
	best := -1
	for i, t := range tasks {
		if t.Status != StatusQueued {
			continue
		}
		if best == -1 || t.CreatedAt.Before(tasks[best].CreatedAt) ||
			(t.CreatedAt.Equal(tasks[best].CreatedAt) && t.ID < tasks[best].ID) {
			best = i
		}
	}
	if best == -1 {
		return Task{}, false
	}
	tasks[best].Status = StatusDownloading
	tasks[best].Stage = ""
	tasks[best].Attempts++
	tasks[best].UpdatedAt = time.Now().UTC()
	if err := s.saveLocked(tasks); err != nil {
		return Task{}, false
	}
	return tasks[best], true
}

// SetStatus moves a task to st and records lastErr.
func (s *Store) SetStatus(id string, st Status, lastErr string) error {
	return s.update(id, func(t *Task) {
		t.Status = st
		if st == StatusFailed || st == StatusQueued || st == StatusPaused {
			t.Stage = ""
		}
		t.LastError = lastErr
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetStage updates a task's current execution stage without changing its status.
func (s *Store) SetStage(id, stage string) error {
	return s.update(id, func(t *Task) {
		t.Stage = stage
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetGroup records the parent-folder group of a task, used to heal a task
// born ungrouped (registration-time resolution failed transiently).
func (s *Store) SetGroup(id, folderID, folderName, group string) error {
	return s.update(id, func(t *Task) {
		t.FolderID = folderID
		t.FolderName = folderName
		t.Group = group
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetDone marks a task done and stores the engine report JSON.
func (s *Store) SetDone(id, reportJSON string) error {
	return s.update(id, func(t *Task) {
		t.Status = StatusDone
		t.Stage = ""
		t.LastError = ""
		t.ReportJSON = reportJSON
		t.UpdatedAt = time.Now().UTC()
	})
}

// RequeueActive moves tasks stuck in downloading/processing back to queued.
// Call it on startup: a crash or hard kill between Claim and SetDone/SetStatus
// otherwise leaves them invisible to workers forever. Attempts are preserved
// so the retry budget resumes where it left off. Returns the requeued count.
func (s *Store) RequeueActive() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return 0, err
	}
	n := 0
	now := time.Now().UTC()
	for i := range tasks {
		if tasks[i].Status != StatusDownloading && tasks[i].Status != StatusProcessing {
			continue
		}
		was := tasks[i].Status
		tasks[i].Status = StatusQueued
		tasks[i].Stage = ""
		if tasks[i].LastError == "" {
			tasks[i].LastError = "requeued after restart (was " + string(was) + ")"
		}
		tasks[i].UpdatedAt = now
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.saveLocked(tasks)
}

// Retry requeues a terminal (or paused) task with a fresh attempt budget.
// skipDownload asks for reconstruct-only: the worker reuses the cached
// archive instead of downloading it again. Returns ErrBadState when the task
// is queued or actively running.
func (s *Store) Retry(id string, skipDownload bool) error {
	return s.updateIf(id, func(t *Task) bool {
		return t.Status == StatusFailed || t.Status == StatusPaused || t.Status == StatusDone
	}, func(t *Task) {
		t.Status = StatusQueued
		t.SkipDownload = skipDownload
		t.Attempts = 0
		t.Stage = ""
		t.LastError = ""
		t.UpdatedAt = time.Now().UTC()
	})
}

// Resume requeues a paused task as-is: attempts, last error and the
// reconstruct-only flag survive, so the pipeline picks up where it stopped.
func (s *Store) Resume(id string) error {
	return s.updateIf(id, func(t *Task) bool { return t.Status == StatusPaused },
		func(t *Task) {
			t.Status = StatusQueued
			t.UpdatedAt = time.Now().UTC()
		})
}

// Pause holds a running task: only downloading/processing tasks can stop.
func (s *Store) Pause(id string) error {
	return s.updateIf(id, func(t *Task) bool {
		return t.Status == StatusDownloading || t.Status == StatusProcessing
	}, func(t *Task) {
		t.Status = StatusPaused
		t.Stage = ""
		t.UpdatedAt = time.Now().UTC()
	})
}

// maxRetainedTasks caps done/failed history so queue.json stays small and fast.
// At ~100 links/2h the default holds roughly a day of terminal rows; older
// ones are pruned on startup (oldest first, regardless of status).
// ponytail: fixed cap; add a RETENTION_DAYS/MAX_TASKS setting if ops wants it.
var maxRetainedTasks = 1200

// PruneTerminal drops the oldest done/failed tasks beyond maxRetainedTasks,
// keeping active work untouched. Returns the dropped count.
func (s *Store) PruneTerminal() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return 0, err
	}
	terminal := 0
	for _, t := range tasks {
		if t.Status == StatusDone || t.Status == StatusFailed {
			terminal++
		}
	}
	drop := terminal - maxRetainedTasks
	if drop <= 0 {
		return 0, nil
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	keep := tasks[:0]
	for _, t := range tasks {
		if drop > 0 && (t.Status == StatusDone || t.Status == StatusFailed) {
			drop--
			continue
		}
		keep = append(keep, t)
	}
	return terminal - maxRetainedTasks, s.saveLocked(keep)
}

// List returns tasks newest-first. A limit <= 0 means no limit.
func (s *Store) List(limit int) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := append([]Task(nil), tasks...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get returns the task with id, or ErrNotFound.
func (s *Store) Get(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return Task{}, err
	}
	for _, t := range tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return Task{}, ErrNotFound
}

// Counts returns the number of tasks in each status (all five keys present).
func (s *Store) Counts() (map[Status]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	m := map[Status]int{
		StatusQueued:      0,
		StatusDownloading: 0,
		StatusProcessing:  0,
		StatusDone:        0,
		StatusFailed:      0,
		StatusPaused:      0,
	}
	for _, t := range tasks {
		m[t.Status]++
	}
	return m, nil
}

// Delete removes a task with id from the store. Returns ErrNotFound if not found.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return err
	}
	idx := -1
	for i, t := range tasks {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrNotFound
	}
	tasks = append(tasks[:idx], tasks[idx+1:]...)
	return s.saveLocked(tasks)
}

// Subscribe registers a callback invoked whenever the store is modified.
func (s *Store) Subscribe(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

// Close releases the store. No-op for the file backend.
func (s *Store) Close() error { return nil }

func (s *Store) update(id string, fn func(*Task)) error {
	return s.updateIf(id, func(*Task) bool { return true }, fn)
}

// updateIf applies fn to task id after checking ok; ok fails with ErrBadState,
// a missing task with ErrNotFound. Persisted (and broadcast) only on success.
func (s *Store) updateIf(id string, ok func(*Task) bool, fn func(*Task)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range tasks {
		if tasks[i].ID == id {
			if !ok(&tasks[i]) {
				return ErrBadState
			}
			fn(&tasks[i])
			return s.saveLocked(tasks)
		}
	}
	return ErrNotFound
}

func (s *Store) loadLocked() ([]Task, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var st diskState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return st.Tasks, nil
}

func (s *Store) saveLocked(tasks []Task) error {
	if tasks == nil {
		tasks = []Task{}
	}
	raw, err := json.MarshalIndent(diskState{Tasks: tasks}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	for _, fn := range s.listeners {
		go fn()
	}
	return nil
}

// newID is timestamp-first with a per-process monotonic counter, so
// lexicographic order matches creation order even on coarse clocks.
func (s *Store) newID(now time.Time) string {
	var b [4]byte
	suffix := ""
	if _, err := rand.Read(b[:]); err == nil {
		suffix = "-" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%020d-%010d%s", now.UnixNano(), s.seq.Add(1), suffix)
}
