// Package sqlite is the SQLite-backed adapter for domain.Repository, selected
// with REPOSITORY_TYPE=sqlite. The database lives at <DATA_DIR>/queue.db.
//
// Concurrency: one *sql.DB with MaxOpenConns(1) plus a sync.Mutex serialises
// every method, matching the file adapter's locking. Writes run in IMMEDIATE
// transactions (_txlock=immediate) so a second process cannot deadlock on a
// read-to-write upgrade.
package sqlite

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/domain"
	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

// Store is a mutex-guarded SQLite task store.
type Store struct {
	mu        sync.Mutex
	db        *sql.DB
	seq       atomic.Uint64
	listeners []func()
}

// tasks columns in scan order; must match scanTask and insertTask.
const cols = `id,url,recording_id,folder_id,folder_name,output_group,status,` +
	`skip_download,stage,attempts,last_error,report_json,created_at,updated_at`

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id            TEXT PRIMARY KEY,
	url           TEXT NOT NULL,
	recording_id  TEXT NOT NULL DEFAULT '',
	folder_id     TEXT NOT NULL DEFAULT '',
	folder_name   TEXT NOT NULL DEFAULT '',
	output_group  TEXT NOT NULL DEFAULT '',
	status        TEXT NOT NULL,
	skip_download INTEGER NOT NULL DEFAULT 0,
	stage         TEXT NOT NULL DEFAULT '',
	attempts      INTEGER NOT NULL DEFAULT 0,
	last_error    TEXT NOT NULL DEFAULT '',
	report_json   TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks(status, created_at);
`

// maxRetainedTasks caps done/failed history, mirroring the file adapter.
var maxRetainedTasks = 1200

// Open creates dataDir if needed and opens (or initialises) queue.db inside it.
// It imports any existing queue.json and renames it to queue.json.migrated, so
// switching backends never loses queued work.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "queue.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Import whenever a legacy queue.json is present, not only when the DB is
	// new: a failed import must not leave the work stranded behind an empty
	// queue.db that makes every later start skip it. INSERT OR IGNORE keeps a
	// retry idempotent.
	n, err := s.importLegacy(filepath.Join(dataDir, "queue.json"))
	if err != nil {
		db.Close()
		return nil, err
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "migrated %d task(s) from queue.json to queue.db (old file kept as queue.json.migrated)\n", n)
	}
	return s, nil
}

// dsn builds the file: URI, percent-encoding the path so a DATA_DIR containing
// '#' or '?' is not parsed as a URI fragment or query.
func dsn(path string) string {
	escaped := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	return "file:" + escaped + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
}

// importLegacy loads tasks from a previous queue.json, renames it aside, and
// returns how many were imported. A missing file is a no-op.
func (s *Store) importLegacy(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var st struct {
		Tasks []domain.Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return 0, fmt.Errorf("migrate %s: %w", path, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	inserted := 0
	for _, t := range st.Tasks {
		res, err := insertTaskOrIgnore(tx, t)
		if err != nil {
			return 0, fmt.Errorf("migrate %s: %w", path, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := os.Rename(path, path+".migrated"); err != nil {
		return inserted, err
	}
	return inserted, nil
}

// Close releases the database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// Subscribe registers a callback invoked whenever the store is modified.
func (s *Store) Subscribe(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

// Enqueue adds a task, or returns the existing active task for the same URL.
func (s *Store) Enqueue(url, recordingID string) (domain.Task, bool, error) {
	return s.EnqueueGrouped(url, recordingID, "", "", "")
}

// EnqueueGrouped is Enqueue with the parent folder already resolved.
func (s *Store) EnqueueGrouped(url, recordingID, folderID, folderName, group string) (domain.Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Task{}, false, err
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT `+cols+` FROM tasks WHERE url=? AND status IN (?,?,?) LIMIT 1`,
		url, domain.StatusQueued, domain.StatusDownloading, domain.StatusProcessing)
	if t, err := scanTask(row); err == nil {
		return t, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, false, err
	}
	now := time.Now().UTC()
	t := domain.Task{ID: s.newID(now), URL: url, RecordingID: recordingID,
		FolderID: folderID, FolderName: folderName, Group: group,
		Status: domain.StatusQueued, CreatedAt: now, UpdatedAt: now}
	if err := insertTask(tx, t); err != nil {
		return domain.Task{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Task{}, false, err
	}
	s.notify()
	return t, false, nil
}

// Claim picks the oldest queued task and marks it downloading.
func (s *Store) Claim() (domain.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Task{}, false
	}
	defer tx.Rollback()
	row := tx.QueryRow(`SELECT `+cols+` FROM tasks WHERE status=? ORDER BY created_at ASC, id ASC LIMIT 1`,
		domain.StatusQueued)
	t, err := scanTask(row)
	if err != nil { // sql.ErrNoRows when the queue is empty, or a real error
		return domain.Task{}, false
	}
	t.Status = domain.StatusDownloading
	t.Stage = ""
	t.Attempts++
	t.UpdatedAt = time.Now().UTC()
	if _, err := tx.Exec(`UPDATE tasks SET status=?, stage=?, attempts=?, updated_at=? WHERE id=?`,
		t.Status, t.Stage, t.Attempts, t.UpdatedAt.UnixNano(), t.ID); err != nil {
		return domain.Task{}, false
	}
	if err := tx.Commit(); err != nil {
		return domain.Task{}, false
	}
	s.notify()
	return t, true
}

// SetStatus moves a task to st and records lastErr.
func (s *Store) SetStatus(id string, st domain.Status, lastErr string) error {
	return s.updateIf(id, always, func(t *domain.Task) {
		t.Status = st
		if st == domain.StatusFailed || st == domain.StatusQueued || st == domain.StatusPaused {
			t.Stage = ""
		}
		t.LastError = lastErr
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetStage updates a task's current execution stage without changing status.
func (s *Store) SetStage(id, stage string) error {
	return s.updateIf(id, always, func(t *domain.Task) {
		t.Stage = stage
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetGroup records the parent-folder group of a task.
func (s *Store) SetGroup(id, folderID, folderName, group string) error {
	return s.updateIf(id, always, func(t *domain.Task) {
		t.FolderID = folderID
		t.FolderName = folderName
		t.Group = group
		t.UpdatedAt = time.Now().UTC()
	})
}

// SetDone marks a task done and stores the engine report JSON.
func (s *Store) SetDone(id, reportJSON string) error {
	return s.updateIf(id, always, func(t *domain.Task) {
		t.Status = domain.StatusDone
		t.Stage = ""
		t.LastError = ""
		t.ReportJSON = reportJSON
		t.UpdatedAt = time.Now().UTC()
	})
}

// Retry requeues a terminal (or paused) task with a fresh attempt budget.
func (s *Store) Retry(id string, skipDownload bool) error {
	return s.updateIf(id, func(t *domain.Task) bool {
		return t.Status == domain.StatusFailed || t.Status == domain.StatusPaused || t.Status == domain.StatusDone
	}, func(t *domain.Task) {
		t.Status = domain.StatusQueued
		t.SkipDownload = skipDownload
		t.Attempts = 0
		t.Stage = ""
		t.LastError = ""
		t.UpdatedAt = time.Now().UTC()
	})
}

// Resume requeues a paused task as-is.
func (s *Store) Resume(id string) error {
	return s.updateIf(id, func(t *domain.Task) bool { return t.Status == domain.StatusPaused },
		func(t *domain.Task) {
			t.Status = domain.StatusQueued
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

// RequeueActive moves tasks stuck in downloading/processing back to queued.
// Call it on startup. Returns the requeued count.
func (s *Store) RequeueActive() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// In SQLite a single UPDATE evaluates every right-hand side against the
	// pre-update row, so the CASE still sees the old status.
	res, err := s.db.Exec(`UPDATE tasks
		SET last_error = CASE WHEN last_error='' THEN 'requeued after restart (was ' || status || ')' ELSE last_error END,
		    status = ?, stage = '', updated_at = ?
		WHERE status IN (?,?)`,
		domain.StatusQueued, time.Now().UTC().UnixNano(), domain.StatusDownloading, domain.StatusProcessing)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.notify()
	}
	return int(n), nil
}

// PruneTerminal drops the oldest done/failed tasks beyond maxRetainedTasks.
func (s *Store) PruneTerminal() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var terminal int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status IN (?,?)`,
		domain.StatusDone, domain.StatusFailed).Scan(&terminal); err != nil {
		return 0, err
	}
	drop := terminal - maxRetainedTasks
	if drop <= 0 {
		return 0, nil
	}
	if _, err := tx.Exec(`DELETE FROM tasks WHERE id IN (
		SELECT id FROM tasks WHERE status IN (?,?) ORDER BY created_at ASC, id ASC LIMIT ?)`,
		domain.StatusDone, domain.StatusFailed, drop); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.notify()
	return drop, nil
}

// List returns tasks newest-first. A limit <= 0 means no limit.
func (s *Store) List(limit int) ([]domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT ` + cols + ` FROM tasks ORDER BY created_at DESC, id DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Get returns the task with id, or domain.ErrNotFound.
func (s *Store) Get(id string) (domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT `+cols+` FROM tasks WHERE id=?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, domain.ErrNotFound
	}
	return t, err
}

// Counts returns the number of tasks in each status (all keys present).
func (s *Store) Counts() (map[domain.Status]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[domain.Status]int{
		domain.StatusQueued: 0, domain.StatusDownloading: 0, domain.StatusProcessing: 0,
		domain.StatusDone: 0, domain.StatusFailed: 0, domain.StatusPaused: 0,
	}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		m[domain.Status(st)] = n
	}
	return m, rows.Err()
}

// Delete removes a task with id. Returns domain.ErrNotFound if absent.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM tasks WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.notify()
	return nil
}

func always(*domain.Task) bool { return true }

// updateIf loads task id inside a transaction, checks ok (ErrBadState) and
// applies fn, then persists the whole row. Missing task -> ErrNotFound.
func (s *Store) updateIf(id string, ok func(*domain.Task) bool, fn func(*domain.Task)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRow(`SELECT `+cols+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !ok(&t) {
		return domain.ErrBadState
	}
	fn(&t)
	if _, err := tx.Exec(`UPDATE tasks SET url=?, recording_id=?, folder_id=?, folder_name=?,
		output_group=?, status=?, skip_download=?, stage=?, attempts=?, last_error=?,
		report_json=?, updated_at=? WHERE id=?`,
		t.URL, t.RecordingID, t.FolderID, t.FolderName, t.Group, t.Status,
		boolInt(t.SkipDownload), t.Stage, t.Attempts, t.LastError, t.ReportJSON,
		t.UpdatedAt.UnixNano(), t.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}

const insertClause = `(` + cols + `) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

func taskArgs(t domain.Task) []any {
	return []any{t.ID, t.URL, t.RecordingID, t.FolderID, t.FolderName, t.Group,
		t.Status, boolInt(t.SkipDownload), t.Stage, t.Attempts, t.LastError,
		t.ReportJSON, t.CreatedAt.UnixNano(), t.UpdatedAt.UnixNano()}
}

// insertTask writes one task inside a transaction.
func insertTask(tx *sql.Tx, t domain.Task) error {
	_, err := tx.Exec(`INSERT INTO tasks `+insertClause, taskArgs(t)...)
	return err
}

// insertTaskOrIgnore is used by the legacy import so retrying after a partial
// migration (commit succeeded, rename failed) does not abort on UNIQUE.
func insertTaskOrIgnore(tx *sql.Tx, t domain.Task) (sql.Result, error) {
	return tx.Exec(`INSERT OR IGNORE INTO tasks `+insertClause, taskArgs(t)...)
}

// scanTask reads one row in cols order.
func scanTask(sc interface{ Scan(...any) error }) (domain.Task, error) {
	var t domain.Task
	var skip int
	var created, updated int64
	err := sc.Scan(&t.ID, &t.URL, &t.RecordingID, &t.FolderID, &t.FolderName, &t.Group,
		&t.Status, &skip, &t.Stage, &t.Attempts, &t.LastError, &t.ReportJSON, &created, &updated)
	if err != nil {
		return domain.Task{}, err
	}
	t.SkipDownload = skip != 0
	t.CreatedAt = time.Unix(0, created).UTC()
	t.UpdatedAt = time.Unix(0, updated).UTC()
	return t, nil
}

// notify fires listeners off the caller's goroutine, like the file adapter.
func (s *Store) notify() {
	for _, fn := range s.listeners {
		go fn()
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// newID is timestamp-first with a per-process monotonic counter; lexicographic
// order matches creation order even on coarse clocks.
func (s *Store) newID(now time.Time) string {
	var b [4]byte
	suffix := ""
	if _, err := rand.Read(b[:]); err == nil {
		suffix = "-" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%020d-%010d%s", now.UnixNano(), s.seq.Add(1), suffix)
}

var _ domain.Repository = (*Store)(nil)
