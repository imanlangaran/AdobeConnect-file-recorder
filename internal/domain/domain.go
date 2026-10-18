// Package domain holds the core entities and the repository ports (interfaces).
//
// It has no dependencies on the outside world: adapters (storage, HTTP, the
// pipeline) depend on it, never the other way round. Swapping the storage
// backend means writing a new Repository implementation, nothing else.
package domain

import (
	"errors"
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
//
// The JSON tags are the persisted form used by the file adapter; the HTTP API
// exposes its own DTO (internal/server.Task) with different casings.
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

// Errors every Repository implementation must be able to return.
var (
	// ErrNotFound is returned when no task with the given ID exists.
	ErrNotFound = errors.New("domain: task not found")
	// ErrBadState is returned for a status transition that is not allowed
	// (e.g. pausing a queued task).
	ErrBadState = errors.New("domain: invalid task state")
)

// Repository is the persistence port for tasks. The file-backed adapter lives
// in internal/repository/filestore; an in-memory or SQL adapter can implement
// this without touching callers.
type Repository interface {
	// Enqueue adds a task, or returns the existing active task for the same
	// URL (dedupe). existed reports whether it predates this call.
	Enqueue(url, recordingID string) (t Task, existed bool, err error)
	// EnqueueGrouped is Enqueue with the parent folder already resolved.
	EnqueueGrouped(url, recordingID, folderID, folderName, group string) (t Task, existed bool, err error)
	// Claim picks the oldest queued task and marks it downloading.
	Claim() (t Task, ok bool)
	SetStatus(id string, st Status, lastErr string) error
	SetStage(id, stage string) error
	SetGroup(id, folderID, folderName, group string) error
	SetDone(id, reportJSON string) error
	// RequeueActive moves interrupted tasks back to queued (call on startup).
	RequeueActive() (int, error)
	// Retry requeues a terminal task; skipDownload reuses the cached archive.
	Retry(id string, skipDownload bool) error
	Resume(id string) error
	Pause(id string) error
	PruneTerminal() (int, error)
	List(limit int) ([]Task, error)
	Get(id string) (Task, error)
	Counts() (map[Status]int, error)
	Delete(id string) error
	// Subscribe registers a callback invoked whenever the store is modified.
	Subscribe(fn func())
	Close() error
}
