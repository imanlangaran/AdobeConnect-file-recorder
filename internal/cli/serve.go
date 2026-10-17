// Package cli implements the ac-rec command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/config"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/engine"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/queue"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/recordingurl"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/server"
)

const (
	idlePoll     = 2 * time.Second
	retryBackoff = 5 * time.Second
)

// runServe runs the queue workers plus the HTTP status page until SIGINT/SIGTERM.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	envPath := fs.String("env", ".env", "path to .env config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "serve takes no positional arguments")
		return 2
	}
	cfg, err := config.Load(*envPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}
	if !engine.HasFFmpeg() {
		fmt.Fprintln(os.Stderr, "error: ffmpeg and ffprobe must be installed and on PATH")
		return 1
	}
	store, err := queue.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open queue: %v\n", err)
		return 1
	}
	defer store.Close()

	if n, err := store.RequeueActive(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: requeue active tasks: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "requeued %d interrupted task(s)\n", n)
	}
	if n, err := store.PruneTerminal(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: prune terminal tasks: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "pruned %d old done/failed task(s)\n", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n := cfg.MaxWorkers
	if n < 1 {
		n = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(ctx, store, cfg)
		}()
	}

	srv := &http.Server{Addr: ":" + strconv.Itoa(cfg.Port), Handler: server.New(queueAdapter{store, cfg})}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	fmt.Fprintf(os.Stderr, "ac-rec serve: %d workers, listening on :%d\n", n, cfg.Port)
	err = srv.ListenAndServe()
	stop()
	wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "error: serve: %v\n", err)
		return 1
	}
	return 0
}

// worker claims tasks until ctx is cancelled. Claim() already marks the task
// downloading and counts the attempt.
func worker(ctx context.Context, store *queue.Store, cfg config.Config) {
	w, h := cfg.ResolutionDims()
	for {
		if ctx.Err() != nil {
			return
		}
		task, ok := store.Claim()
		if !ok {
			if !sleepCtx(ctx, idlePoll) {
				return
			}
			continue
		}
		processTask(ctx, store, cfg, task, w, h)
	}
}

func processTask(ctx context.Context, store *queue.Store, cfg config.Config, task queue.Task, w, h int) {
	var lastStageUpdate time.Time
	var lastStageStr string
	setStage := func(s string, force bool) {
		now := time.Now()
		if !force && s == lastStageStr && now.Sub(lastStageUpdate) < 1*time.Second {
			return
		}
		lastStageUpdate = now
		lastStageStr = s
		_ = store.SetStage(task.ID, s)
	}
	status := func(s string) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", task.ID, s)
		setStage(s, true)
	}
	ru, err := recordingurl.Parse(task.URL)
	if err != nil {
		failTask(ctx, store, cfg, task, err)
		return
	}
	// Output name is the recording's real title (the page <title>); the URL
	// slug only backs it up when the page is unreachable/unauthorized.
	title := fetchTitle(ctx, ru.PageURL(), "", nil, cfg.ACUsername, cfg.ACPassword)
	if title == "" {
		title = task.RecordingID
	}
	if title == "" {
		title = ru.ID
	}
	// Heal a task born ungrouped: registration-time resolution failed
	// transiently, and nothing ever retried it. The worker owns the task
	// exclusively here, so re-resolving before the run is race-free.
	if task.Group == "" || task.Group == unfiledGroup {
		id, name := resolveFolder(ctx, ru.PageURL(), cfg.ACUsername, cfg.ACPassword)
		g := ensureGroup(cfg.OutDir, id, name)
		if g != unfiledGroup {
			task.Group = g
			if err := store.SetGroup(task.ID, id, name, g); err != nil {
				status(fmt.Sprintf("set group: %v", err))
			}
			status(fmt.Sprintf("folder resolved on retry: %s", g))
		}
	}
	if err := store.SetStatus(task.ID, queue.StatusProcessing, ""); err != nil {
		status(fmt.Sprintf("set processing: %v", err))
	}
	setStage("starting", true)
	cache := filepath.Join(cfg.DataDir, "cache", task.ID+".zip")
	rep, err := engine.Run(ctx, engine.Options{
		ZipURL: ru.ZipURL(),
		Title:  title,
		// Parent folder resolved at registration ("", never for new tasks:
		// unresolved ones are explicitly _unfiled).
		Group:    task.Group,
		OutDir:   cfg.OutDir,
		MP3:      cfg.ExportMP3,
		Width:    w,
		Height:   h,
		FPS:      cfg.DefaultFPS,
		Preset:   cfg.DefaultPreset,
		CRF:      cfg.DefaultCRF,
		Username: cfg.ACUsername,
		Password: cfg.ACPassword,
		// Kept across attempts: a retry after a reconstruct failure skips the
		// re-download. engine.Run only reuses it when it holds a valid archive.
		CachePath: cache,
		Status:    status,
		Progress: func(stage string, done, total int64) {
			setStage(formatStage(stage, done, total), false)
		},
		// Retries must start clean: a crash after recording.mp4 was written
		// would otherwise fail with "output already exists".
		Overwrite: true,
	})
	if err != nil {
		failTask(ctx, store, cfg, task, err)
		return
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		failTask(ctx, store, cfg, task, err)
		return
	}
	if err := store.SetDone(task.ID, string(raw)); err != nil {
		status(fmt.Sprintf("set done: %v", err))
		return
	}
	_ = os.Remove(cache) // done: the archive is no longer needed for retries
	status(fmt.Sprintf("done: %s", rep.VideoPath))
}

// formatStage renders a human-friendly progress label for the dashboard.
func formatStage(stage string, done, total int64) string {
	if stage == "download" || stage == "extract" {
		if total > 0 {
			return fmt.Sprintf("%s %.0f%% (%.1f/%.1f MB)", stage,
				float64(done)/float64(total)*100,
				float64(done)/(1<<20), float64(total)/(1<<20))
		}
		return fmt.Sprintf("%s %.1f MB", stage, float64(done)/(1<<20))
	}
	if total > 0 {
		return fmt.Sprintf("%s %.0f%% (%s/%s)", stage,
			float64(done)/float64(total)*100, fmtDur(done), fmtDur(total))
	}
	return fmt.Sprintf("%s %s", stage, fmtDur(done))
}

// failTask requeues while attempts remain (Claim counts them), else fails.
// ponytail: fixed 5s backoff; add jitter/exponential growth if retries hammer the server.
func failTask(ctx context.Context, store *queue.Store, cfg config.Config, task queue.Task, err error) {
	fmt.Fprintf(os.Stderr, "[%s] failed (attempt %d): %v\n", task.ID, task.Attempts, err)
	_ = store.SetStage(task.ID, "")
	if ctx.Err() == nil && task.Attempts > cfg.RetryCount {
		if serr := store.SetStatus(task.ID, queue.StatusFailed, err.Error()); serr != nil {
			fmt.Fprintf(os.Stderr, "[%s] set failed: %v\n", task.ID, serr)
		}
		return
	}
	if ctx.Err() == nil { // running normally -> backoff before requeue
		sleepCtx(ctx, retryBackoff)
	} // shutting down -> requeue immediately for the next run
	if serr := store.SetStatus(task.ID, queue.StatusQueued, err.Error()); serr != nil {
		fmt.Fprintf(os.Stderr, "[%s] requeue: %v\n", task.ID, serr)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// queueAdapter bridges *queue.Store to server.Store (server keeps local
// mirrors of the queue types; see its package comment).
type queueAdapter struct {
	q   *queue.Store
	cfg config.Config
}

func (a queueAdapter) Enqueue(url, recordingID string) (server.Task, bool, error) {
	// Resolve the parent folder BEFORE the task is persisted: a task saved
	// without its group can be claimed by a worker in the meantime and would
	// output flat instead of into its class folder.
	folderID, folderName, group := a.resolve(url)
	// queue.Enqueue reports dedupe hits atomically under its own lock.
	t, existed, err := a.q.EnqueueGrouped(url, recordingID, folderID, folderName, group)
	if err != nil {
		return server.Task{}, false, err
	}
	return toServerTask(t), existed, nil
}

// resolve maps a recording URL to its parent folder (meeting SCO) and creates
// the output directory. Best-effort: failures yield empty values and the
// recording lands in the _unfiled group.
func (a queueAdapter) resolve(url string) (folderID, folderName, group string) {
	if a.cfg.ACUsername == "" {
		return "", "", "" // no credentials: cannot resolve, recording would not download either
	}
	// The HTTP handler must not block on the API for long, but the budget
	// must still cover the whole 4-request chain (see resolveBudget).
	ctx, cancel := context.WithTimeout(context.Background(), resolveBudget)
	defer cancel()
	ru, err := recordingurl.Parse(url)
	if err != nil {
		return "", "", ""
	}
	folderID, folderName = resolveFolder(ctx, ru.PageURL(), a.cfg.ACUsername, a.cfg.ACPassword)
	return folderID, folderName, ensureGroup(a.cfg.OutDir, folderID, folderName)
}

func (a queueAdapter) Get(id string) (server.Task, bool) {
	t, err := a.q.Get(id)
	if err != nil {
		return server.Task{}, false
	}
	return toServerTask(t), true
}

func (a queueAdapter) List(limit int) []server.Task {
	tasks, err := a.q.List(limit)
	if err != nil {
		return nil
	}
	out := make([]server.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toServerTask(t))
	}
	return out
}

func (a queueAdapter) Counts() map[string]int {
	m, err := a.q.Counts()
	if err != nil {
		return map[string]int{}
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

func (a queueAdapter) Delete(id string) bool {
	return a.q.Delete(id) == nil
}

func (a queueAdapter) Subscribe(fn func()) {
	a.q.Subscribe(fn)
}

func toServerTask(t queue.Task) server.Task {
	return server.Task{
		ID: t.ID, URL: t.URL, RecordingID: t.RecordingID,
		Status: string(t.Status), Stage: t.Stage, Attempts: t.Attempts,
		LastError: t.LastError, ReportJSON: t.ReportJSON,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}
