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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/config"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/domain"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/engine"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/recordingurl"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/repository/filestore"
	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/repository/sqlite"
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
	store, err := openRepository(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open %s repository: %v\n", cfg.RepositoryType, err)
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

	dlWorkers := cfg.MaxDownloadWorkers
	if dlWorkers < 1 {
		dlWorkers = 1
	}
	rcWorkers := cfg.MaxReconstructWorkers
	if rcWorkers < 1 {
		rcWorkers = 1
	}
	var wg sync.WaitGroup
	rs := newRuns()
	for i := 0; i < dlWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			downloadWorker(ctx, store, cfg, rs)
		}()
	}
	for i := 0; i < rcWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reconstructWorker(ctx, store, cfg, rs)
		}()
	}

	srv := &http.Server{Addr: ":" + strconv.Itoa(cfg.Port), Handler: server.New(queueAdapter{store, cfg, rs})}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	fmt.Fprintf(os.Stderr, "ac-rec serve: %d download workers, %d reconstruct workers, listening on :%d\n", dlWorkers, rcWorkers, cfg.Port)
	err = srv.ListenAndServe()
	stop()
	wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "error: serve: %v\n", err)
		return 1
	}
	return 0
}

// openRepository picks the storage backend named by REPOSITORY_TYPE.
func openRepository(cfg config.Config) (domain.Repository, error) {
	if cfg.RepositoryType == "sqlite" {
		return sqlite.Open(cfg.DataDir)
	}
	return filestore.Open(cfg.DataDir)
}

// downloadWorker claims queued tasks, downloads their archives to cache, and marks them downloaded.
func downloadWorker(ctx context.Context, store domain.Repository, cfg config.Config, rs *runs) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Backpressure guard: pause claiming downloads if downloaded backlog is high
		// ponytail: fixed threshold 2*MaxReconstructWorkers; add MAX_DOWNLOAD_QUEUE if ops wants.
		backlogCap := 2 * cfg.MaxReconstructWorkers
		if backlogCap < 4 {
			backlogCap = 4
		}
		counts, err := store.Counts()
		if err == nil && counts[domain.StatusDownloaded] >= backlogCap {
			if !sleepCtx(ctx, idlePoll) {
				return
			}
			continue
		}

		task, ok := store.ClaimDownload()
		if !ok {
			if !sleepCtx(ctx, idlePoll) {
				return
			}
			continue
		}
		processDownload(ctx, store, cfg, task, rs)
	}
}

// reconstructWorker claims downloaded tasks, reconstructs media via ffmpeg, and marks them done.
func reconstructWorker(ctx context.Context, store domain.Repository, cfg config.Config, rs *runs) {
	w, h := cfg.ResolutionDims()
	for {
		if ctx.Err() != nil {
			return
		}
		task, ok := store.ClaimReconstruct()
		if !ok {
			if !sleepCtx(ctx, idlePoll) {
				return
			}
			continue
		}
		processReconstruct(ctx, store, cfg, task, w, h, rs)
	}
}

// run is one in-flight task; stopped records a user stop so the worker can
// tell it apart from a shutdown or a plain failure.
type run struct {
	cancel  context.CancelFunc
	stopped atomic.Bool
}

// runs maps task ID -> in-flight run so the API can cancel a single task.
type runs struct {
	mu sync.Mutex
	m  map[string]*run
}

func newRuns() *runs { return &runs{m: make(map[string]*run)} }

func (r *runs) add(id string, cancel context.CancelFunc) *run {
	rh := &run{cancel: cancel}
	r.mu.Lock()
	r.m[id] = rh
	r.mu.Unlock()
	return rh
}

func (r *runs) get(id string) *run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *runs) remove(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

func processDownload(ctx context.Context, store domain.Repository, cfg config.Config, task domain.Task, rs *runs) {
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
	setStage("downloading archive", true)
	cache := filepath.Join(cfg.DataDir, "cache", task.ID+".zip")
	runCtx, cancel := context.WithCancel(ctx)
	rh := rs.add(task.ID, cancel)
	defer func() {
		cancel()
		rs.remove(task.ID)
	}()
	opts := engine.Options{
		ZipURL:    ru.ZipURL(),
		CachePath: cache,
		Username:  cfg.ACUsername,
		Password:  cfg.ACPassword,
		Status:    status,
		Progress: func(stage string, done, total int64) {
			setStage(formatStage(stage, done, total), false)
		},
	}
	_, _, err = engine.DownloadArchive(runCtx, opts)
	if err != nil {
		if rh.stopped.Load() { // user stop: hold, don't burn the retry budget
			if serr := store.SetStatus(task.ID, domain.StatusPaused, "stopped by user"); serr != nil {
				fmt.Fprintf(os.Stderr, "[%s] set paused: %v\n", task.ID, serr)
			}
			status("stopped by user")
			return
		}
		failTask(ctx, store, cfg, task, err)
		return
	}
	if err := store.SetStatus(task.ID, domain.StatusDownloaded, ""); err != nil {
		status(fmt.Sprintf("set downloaded: %v", err))
		return
	}
	setStage("ready for transcode", true)
	status("download complete")
}

func processReconstruct(ctx context.Context, store domain.Repository, cfg config.Config, task domain.Task, w, h int, rs *runs) {
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
	cache := filepath.Join(cfg.DataDir, "cache", task.ID+".zip")
	if !engine.HasCachedArchive(cache) {
		failReconstructTask(ctx, store, cfg, task, errors.New("cached archive missing or corrupted"), cache)
		return
	}
	ru, err := recordingurl.Parse(task.URL)
	if err != nil {
		failReconstructTask(ctx, store, cfg, task, err, cache)
		return
	}
	title := fetchTitle(ctx, ru.PageURL(), "", nil, cfg.ACUsername, cfg.ACPassword)
	if title == "" {
		title = task.RecordingID
	}
	if title == "" {
		title = ru.ID
	}
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
	setStage("starting transcode", true)
	runCtx, cancel := context.WithCancel(ctx)
	rh := rs.add(task.ID, cancel)
	defer func() {
		cancel()
		rs.remove(task.ID)
	}()
	opts := engine.Options{
		Title:     title,
		Group:     task.Group,
		OutDir:    cfg.OutDir,
		MP3:       cfg.ExportMP3,
		Width:     w,
		Height:    h,
		FPS:       cfg.DefaultFPS,
		Preset:    cfg.DefaultPreset,
		CRF:       cfg.DefaultCRF,
		CachePath: cache,
		Status:    status,
		Progress: func(stage string, done, total int64) {
			setStage(formatStage(stage, done, total), false)
		},
		Overwrite: true,
	}
	rep, err := engine.ReconstructArchive(runCtx, opts, cache)
	if err != nil {
		if rh.stopped.Load() {
			if serr := store.SetStatus(task.ID, domain.StatusPaused, "stopped by user"); serr != nil {
				fmt.Fprintf(os.Stderr, "[%s] set paused: %v\n", task.ID, serr)
			}
			status("stopped by user")
			return
		}
		failReconstructTask(ctx, store, cfg, task, err, cache)
		return
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		failReconstructTask(ctx, store, cfg, task, err, cache)
		return
	}
	if err := store.SetDone(task.ID, string(raw)); err != nil {
		status(fmt.Sprintf("set done: %v", err))
		return
	}
	_ = os.Remove(cache)
	status(fmt.Sprintf("done: %s", rep.VideoPath))
}

func failReconstructTask(ctx context.Context, store domain.Repository, cfg config.Config, task domain.Task, err error, cache string) {
	fmt.Fprintf(os.Stderr, "[%s] reconstruct failed (attempt %d): %v\n", task.ID, task.Attempts, err)
	_ = store.SetStage(task.ID, "")
	if ctx.Err() == nil && task.Attempts > cfg.RetryCount {
		if serr := store.SetStatus(task.ID, domain.StatusFailed, err.Error()); serr != nil {
			fmt.Fprintf(os.Stderr, "[%s] set failed: %v\n", task.ID, serr)
		}
		return
	}
	if ctx.Err() == nil {
		sleepCtx(ctx, retryBackoff)
	}
	targetStatus := domain.StatusDownloaded
	if !engine.HasCachedArchive(cache) {
		targetStatus = domain.StatusQueued
	}
	if serr := store.SetStatus(task.ID, targetStatus, err.Error()); serr != nil {
		fmt.Fprintf(os.Stderr, "[%s] requeue: %v\n", task.ID, serr)
	}
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
func failTask(ctx context.Context, store domain.Repository, cfg config.Config, task domain.Task, err error) {
	fmt.Fprintf(os.Stderr, "[%s] failed (attempt %d): %v\n", task.ID, task.Attempts, err)
	_ = store.SetStage(task.ID, "")
	if ctx.Err() == nil && task.Attempts > cfg.RetryCount {
		if serr := store.SetStatus(task.ID, domain.StatusFailed, err.Error()); serr != nil {
			fmt.Fprintf(os.Stderr, "[%s] set failed: %v\n", task.ID, serr)
		}
		return
	}
	if ctx.Err() == nil { // running normally -> backoff before requeue
		sleepCtx(ctx, retryBackoff)
	} // shutting down -> requeue immediately for the next run
	if serr := store.SetStatus(task.ID, domain.StatusQueued, err.Error()); serr != nil {
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

// queueAdapter bridges domain.Repository to server.Store (server defines its
// own HTTP DTO; see its package comment).
type queueAdapter struct {
	q   domain.Repository
	cfg config.Config
	rs  *runs
}

// Retry requeues a failed/paused/done task. mode:
//   - "full": drop the cached archive, re-download, reconstruct;
//   - "reconstruct": reuse the cached archive (rejected when there is none);
//   - "resume": paused task back to work as-is (attempts/cache kept).
func (a queueAdapter) Retry(id, mode string) error {
	cache := filepath.Join(a.cfg.DataDir, "cache", id+".zip")
	switch mode {
	case "resume":
		return toServerErr(a.q.Resume(id))
	case "reconstruct":
		if !engine.HasCachedArchive(cache) {
			return errors.New("no cached archive for this task - use full retry")
		}
		return toServerErr(a.q.Retry(id, true))
	default: // "full"
		// State check first: don't drop the cache of a task Retry would
		// reject (queued/running -> ErrBadState).
		if _, err := a.q.Get(id); err != nil {
			return toServerErr(err)
		}
		if err := os.Remove(cache); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return toServerErr(a.q.Retry(id, false))
	}
}

// toServerErr translates domain errors into the HTTP layer's sentinels.
func toServerErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return server.ErrNotFound
	case errors.Is(err, domain.ErrBadState):
		return server.ErrBadState
	}
	return err
}

// Stop cancels an in-flight task and parks it in paused. False when the task
// is not running (nothing to stop).
func (a queueAdapter) Stop(id string) bool {
	rh := a.rs.get(id)
	if rh == nil {
		return false
	}
	rh.stopped.Store(true)
	rh.cancel()
	// The worker's error path also lands on paused; Pause here so the UI
	// reflects the stop immediately.
	_ = a.q.Pause(id)
	return true
}

func (a queueAdapter) WorkerLimits() (int, int) {
	dl := a.cfg.MaxDownloadWorkers
	if dl < 1 {
		dl = 1
	}
	rc := a.cfg.MaxReconstructWorkers
	if rc < 1 {
		rc = 1
	}
	return dl, rc
}

func (a queueAdapter) Enqueue(url, recordingID string) (server.Task, bool, error) {
	// Resolve the parent folder BEFORE the task is persisted: a task saved
	// without its group can be claimed by a worker in the meantime and would
	// output flat instead of into its class folder.
	folderID, folderName, group := a.resolve(url)
	// domain.Enqueue reports dedupe hits atomically under its own lock.
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

func toServerTask(t domain.Task) server.Task {
	return server.Task{
		ID: t.ID, URL: t.URL, RecordingID: t.RecordingID,
		FolderID: t.FolderID, FolderName: t.FolderName, Group: t.Group,
		Status: string(t.Status), SkipDownload: t.SkipDownload,
		Stage: t.Stage, Attempts: t.Attempts,
		LastError: t.LastError, ReportJSON: t.ReportJSON,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}
