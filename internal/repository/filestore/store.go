// Package filestore is the file-backed adapter for domain.Repository: a
// mutex-guarded JSON file written atomically (temp file + rename) so a crash
// never leaves a half-written queue behind.
//
// This is the default backend (REPOSITORY_TYPE=file). For a large queue or
// multi-process access, use the SQLite adapter in internal/repository/sqlite
// (REPOSITORY_TYPE=sqlite); it migrates an existing queue.json on first open.
package filestore

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

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/domain"
)

// Store is a mutex-guarded, file-backed task queue. Every mutation is
// persisted atomically (write temp file + rename) so a crash never leaves a
// half-written queue behind. Safe for concurrent use within one process.
type Store struct {
	mu        sync.Mutex
	dir       string
	path      string
	seq       atomic.Uint64
	listeners []func()
}

type diskState struct {
	Tasks []domain.Task `json:"tasks"`
}

// Open creates dataDir if needed and opens (or initialises) queue.json inside it.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dataDir, path: filepath.Join(dataDir, "queue.json")}
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
func (s *Store) Enqueue(url, recordingID string) (domain.Task, bool, error) {
	return s.EnqueueGrouped(url, recordingID, "", "", "")
}

// EnqueueGrouped registers a task already resolved to its parent folder. The
// group must be set here, not after: a task persisted ungrouped can be claimed
// by a worker before a later SetGroup and would output to the wrong directory.
func (s *Store) EnqueueGrouped(url, recordingID, folderID, folderName, group string) (t domain.Task, existed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return domain.Task{}, false, err
	}
	for _, t := range tasks {
		if t.URL == url && (t.Status == domain.StatusQueued || t.Status == domain.StatusDownloading || t.Status == domain.StatusDownloaded || t.Status == domain.StatusProcessing) {
			return t, true, nil
		}
	}
	now := time.Now().UTC()
	t = domain.Task{ID: s.newID(now), URL: url, RecordingID: recordingID,
		FolderID: folderID, FolderName: folderName, Group: group,
		Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now}
	tasks = append(tasks, t)
	if err := s.saveLocked(tasks); err != nil {
		return domain.Task{}, false, err
	}
	return t, false, nil
}

// ClaimDownload picks the oldest queued task and marks it downloading.
// The second return value is false when no queued task exists.
func (s *Store) ClaimDownload() (domain.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return domain.Task{}, false
	}
	best := -1
	for i, t := range tasks {
		if t.Status != domain.StatusQueued {
			continue
		}
		if best == -1 || t.CreatedAt.Before(tasks[best].CreatedAt) ||
			(t.CreatedAt.Equal(tasks[best].CreatedAt) && t.ID < tasks[best].ID) {
			best = i
		}
	}
	if best == -1 {
		return domain.Task{}, false
	}
	tasks[best].Status = domain.StatusDownloading
	tasks[best].Stage = ""
	tasks[best].Attempts++
	tasks[best].UpdatedAt = time.Now().UTC()
	if err := s.saveLocked(tasks); err != nil {
		return domain.Task{}, false
	}
	return tasks[best], true
}

// ClaimReconstruct picks the oldest downloaded task and marks it processing.
// The second return value is false when no downloaded task exists.
func (s *Store) ClaimReconstruct() (domain.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return domain.Task{}, false
	}
	best := -1
	for i, t := range tasks {
		if t.Status != domain.StatusDownloaded {
			continue
		}
		if best == -1 || t.CreatedAt.Before(tasks[best].CreatedAt) ||
			(t.CreatedAt.Equal(tasks[best].CreatedAt) && t.ID < tasks[best].ID) {
			best = i
		}
	}
	if best == -1 {
		return domain.Task{}, false
	}
	tasks[best].Status = domain.StatusProcessing
	tasks[best].Stage = ""
	tasks[best].UpdatedAt = time.Now().UTC()
	if err := s.saveLocked(tasks); err != nil {
		return domain.Task{}, false
	}
	return tasks[best], true
}

// SetStatus moves a task to st and records lastErr.
func (s *Store) SetStatus(id string, st domain.Status, lastErr string) error {
	return s.update(id, func(t *domain.Task) {
		t.Status = st
		if st == domain.StatusFailed || st == domain.StatusQueued || st == domain.StatusDownloaded || st == domain.StatusPaused {
			t.Stage = ""
		}
		t.LastError = lastErr
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetStage updates a task's current execution stage without changing its status.
func (s *Store) SetStage(id, stage string) error {
	return s.update(id, func(t *domain.Task) {
		t.Stage = stage
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetGroup records the parent-folder group of a task, used to heal a task
// born ungrouped (registration-time resolution failed transiently).
func (s *Store) SetGroup(id, folderID, folderName, group string) error {
	return s.update(id, func(t *domain.Task) {
		t.FolderID = folderID
		t.FolderName = folderName
		t.Group = group
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetDone marks a task done and stores the engine report JSON.
func (s *Store) SetDone(id, reportJSON string) error {
	return s.update(id, func(t *domain.Task) {
		t.Status = domain.StatusDone
		t.Stage = ""
		t.LastError = ""
		t.ReportJSON = reportJSON
		t.UpdatedAt = time.Now().UTC()
	})
}

// RequeueActive moves interrupted tasks back to an actionable state on startup:
// downloading -> queued, processing -> downloaded (if cached archive exists) or queued.
// Tasks in downloaded remain downloaded. Attempts are preserved. Returns the requeued count.
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
		if tasks[i].Status != domain.StatusDownloading && tasks[i].Status != domain.StatusProcessing {
			continue
		}
		was := tasks[i].Status
		if was == domain.StatusProcessing {
			archive := filepath.Join(s.dir, "cache", tasks[i].ID+".zip")
			if _, err := os.Stat(archive); err == nil {
				tasks[i].Status = domain.StatusDownloaded
			} else {
				tasks[i].Status = domain.StatusQueued
			}
		} else {
			tasks[i].Status = domain.StatusQueued
		}
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
// skipDownload sets StatusDownloaded (reconstruct-only); false sets StatusQueued.
// Returns domain.ErrBadState when the task is queued or actively running.
func (s *Store) Retry(id string, skipDownload bool) error {
	return s.updateIf(id, func(t *domain.Task) bool {
		return t.Status == domain.StatusFailed || t.Status == domain.StatusPaused || t.Status == domain.StatusDone
	}, func(t *domain.Task) {
		if skipDownload {
			t.Status = domain.StatusDownloaded
		} else {
			t.Status = domain.StatusQueued
		}
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
	return s.updateIf(id, func(t *domain.Task) bool { return t.Status == domain.StatusPaused },
		func(t *domain.Task) {
			if t.SkipDownload {
				t.Status = domain.StatusDownloaded
			} else {
				t.Status = domain.StatusQueued
			}
			t.UpdatedAt = time.Now().UTC()
		})
}

// Pause holds a running task: only downloading/processing tasks can stop.
func (s *Store) Pause(id string) error {
	return s.updateIf(id, func(t *domain.Task) bool {
		return t.Status == domain.StatusDownloading || t.Status == domain.StatusProcessing
	}, func(t *domain.Task) {
		t.Status = domain.StatusPaused
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
		if t.Status == domain.StatusDone || t.Status == domain.StatusFailed {
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
		if drop > 0 && (t.Status == domain.StatusDone || t.Status == domain.StatusFailed) {
			drop--
			continue
		}
		keep = append(keep, t)
	}
	return terminal - maxRetainedTasks, s.saveLocked(keep)
}

// List returns tasks newest-first. A limit <= 0 means no limit.
func (s *Store) List(limit int) ([]domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := append([]domain.Task(nil), tasks...)
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

// Get returns the task with id, or domain.ErrNotFound.
func (s *Store) Get(id string) (domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return domain.Task{}, err
	}
	for _, t := range tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return domain.Task{}, domain.ErrNotFound
}

// Counts returns the number of tasks in each status (all five keys present).
func (s *Store) Counts() (map[domain.Status]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	m := map[domain.Status]int{
		domain.StatusQueued:      0,
		domain.StatusDownloading: 0,
		domain.StatusDownloaded:  0,
		domain.StatusProcessing:  0,
		domain.StatusDone:        0,
		domain.StatusFailed:      0,
		domain.StatusPaused:      0,
	}
	for _, t := range tasks {
		m[t.Status]++
	}
	return m, nil
}

// Delete removes a task with id from the store. Returns domain.ErrNotFound if not found.
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
		return domain.ErrNotFound
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

func (s *Store) update(id string, fn func(*domain.Task)) error {
	return s.updateIf(id, func(*domain.Task) bool { return true }, fn)
}

// updateIf applies fn to task id after checking ok; ok fails with domain.ErrBadState,
// a missing task with domain.ErrNotFound. Persisted (and broadcast) only on success.
func (s *Store) updateIf(id string, ok func(*domain.Task) bool, fn func(*domain.Task)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.loadLocked()
	if err != nil {
		return err
	}
	for i := range tasks {
		if tasks[i].ID == id {
			if !ok(&tasks[i]) {
				return domain.ErrBadState
			}
			fn(&tasks[i])
			return s.saveLocked(tasks)
		}
	}
	return domain.ErrNotFound
}

func (s *Store) loadLocked() ([]domain.Task, error) {
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

func (s *Store) saveLocked(tasks []domain.Task) error {
	if tasks == nil {
		tasks = []domain.Task{}
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

var _ domain.Repository = (*Store)(nil)
