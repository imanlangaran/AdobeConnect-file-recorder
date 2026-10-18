// Package server exposes the queue over HTTP with a status page.
// The page is a Tailwind/daisyUI/htmx UI served from static CDNs (no Go
// dependencies); Store/Task are local mirrors of the queue API, bridged in
// production by cli.queueAdapter.
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/recordingurl"
)

// Task mirrors queue.Task.
type Task struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	RecordingID  string    `json:"recordingID"`
	FolderID     string    `json:"folderID,omitempty"`   // parent meeting SCO id
	FolderName   string    `json:"folderName,omitempty"` // human name of that folder
	Group        string    `json:"group,omitempty"`      // output subdir: recordings/<group>/
	Status       string    `json:"status"`
	SkipDownload bool      `json:"skipDownload,omitempty"` // reconstruct-only retry pending
	Stage        string    `json:"stage,omitempty"`
	Attempts     int       `json:"attempts"`
	LastError    string    `json:"lastError"`
	ReportJSON   string    `json:"reportJSON,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Store is the subset of queue.Store the HTTP layer needs.
type Store interface {
	Enqueue(url, recordingID string) (t Task, existed bool, err error)
	Get(id string) (t Task, ok bool)
	List(limit int) []Task
	Counts() map[string]int
	Delete(id string) bool
	// Retry requeues a failed/paused/done task. mode: full | reconstruct | resume.
	Retry(id, mode string) error
	// Stop cancels an in-flight task and pauses it; false when not running.
	Stop(id string) bool
}

// Subscriber is an optional interface for stores supporting mutation callbacks.
type Subscriber interface {
	Subscribe(func())
}

// Sentinel errors Store implementations translate queue errors into, so the
// HTTP layer maps them to statuses without importing the queue package.
var (
	ErrNotFound = errors.New("server: task not found")
	ErrBadState = errors.New("server: invalid task state")
)

type hub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[chan struct{}]struct{})}
}

func (h *hub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *hub) register(ch chan struct{}) {
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) unregister(ch chan struct{}) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

// New wires the routes and returns the handler.
func New(store Store) http.Handler {
	h := newHub()
	if sub, ok := store.(Subscriber); ok {
		sub.Subscribe(h.broadcast)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(r.FormValue("url"))
		if raw == "" && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				URL string `json:"url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				respondTaskErr(w, r, http.StatusBadRequest, "invalid JSON body")
				return
			}
			raw = strings.TrimSpace(body.URL)
		}
		if raw == "" {
			respondTaskErr(w, r, http.StatusBadRequest, "url is required")
			return
		}
		rec, err := recordingurl.Parse(raw)
		if err != nil {
			respondTaskErr(w, r, http.StatusBadRequest, "invalid recording URL")
			return
		}
		t, existed, err := store.Enqueue(raw, rec.ID)
		if err != nil {
			respondTaskErr(w, r, http.StatusInternalServerError, "enqueue failed")
			return
		}
		h.broadcast()
		code := http.StatusAccepted
		if existed {
			code = http.StatusOK
		}
		respondTaskOK(w, r, code, t, existed)
	})
	mux.HandleFunc("DELETE /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !store.Delete(id) {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		h.broadcast()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
	})
	mux.HandleFunc("POST /api/tasks/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mode := r.URL.Query().Get("mode")
		if mode == "" && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			var body struct {
				Mode string `json:"mode"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mode = body.Mode
		}
		if mode == "" {
			mode = "full"
		}
		if mode != "full" && mode != "reconstruct" && mode != "resume" {
			writeErr(w, http.StatusBadRequest, "mode must be full, reconstruct or resume")
			return
		}
		if err := store.Retry(id, mode); err != nil {
			respondRetryErr(w, err)
			return
		}
		h.broadcast()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "retried": id, "mode": mode})
	})
	mux.HandleFunc("POST /api/tasks/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !store.Stop(id) {
			writeErr(w, http.StatusConflict, "task is not running")
			return
		}
		h.broadcast()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": id})
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch := make(chan struct{}, 1)
		h.register(ch)
		defer h.unregister(ch)

		fmt.Fprintf(w, "event: ping\ndata: connected\n\n")
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				fmt.Fprintf(w, "event: update\ndata: {}\n\n")
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}
		tasks := store.List(limit)
		if tasks == nil {
			tasks = []Task{}
		}
		writeJSON(w, http.StatusOK, tasks)
	})
	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := store.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, t)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /partials/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashTmpl.Execute(w, snapshot(store))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeStatusPage(w, store)
	})
	return mux
}

type dashboard struct {
	Counts map[string]int
	Tasks  []Task
}

func snapshot(store Store) dashboard {
	d := dashboard{Counts: store.Counts(), Tasks: store.List(200)}
	if d.Counts == nil {
		d.Counts = map[string]int{}
	}
	if d.Tasks == nil {
		d.Tasks = []Task{}
	}
	return d
}

func writeStatusPage(w http.ResponseWriter, store Store) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, snapshot(store)); err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
	}
}

// htmx reports itself via the HX-Request header; plain API clients don't.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// respondTaskOK answers POST /api/tasks: HTML alert fragment for the page
// form, JSON task for API clients. HX-Reswap lets htmx honor the 202/200.
func respondTaskOK(w http.ResponseWriter, r *http.Request, code int, t Task, existed bool) {
	if !isHTMX(r) {
		writeJSON(w, code, t)
		return
	}
	kind := "alert-success"
	msg := fmt.Sprintf("Queued <b>%s</b> <span class=\"opacity-70\">(%s)</span>", template.HTMLEscapeString(t.RecordingID), template.HTMLEscapeString(shortID(t.ID)))
	if existed {
		kind = "alert-info"
		msg = fmt.Sprintf("Already in queue: <b>%s</b> <span class=\"opacity-70\">(%s, %s)</span>", template.HTMLEscapeString(t.RecordingID), template.HTMLEscapeString(shortID(t.ID)), template.HTMLEscapeString(t.Status))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("HX-Retarget", "#form-msg")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<div class="alert %s py-2 text-sm shadow"><span>%s</span></div>`, kind, msg)
}

func respondTaskErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if !isHTMX(r) {
		writeErr(w, code, msg)
		return
	}
	// htmx swaps only 2xx by default, so errors ride a 200 with an
	// error-styled fragment; API clients still get the real status above.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("HX-Retarget", "#form-msg")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<div class="alert alert-error py-2 text-sm shadow"><span>%s</span></div>`, template.HTMLEscapeString(msg))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// respondRetryErr maps retry/stop store errors to 404/409 responses.
func respondRetryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "task not found")
	case errors.Is(err, ErrBadState):
		writeErr(w, http.StatusConflict, "task is not in a retryable state")
	default: // e.g. "no cached archive ... use full retry"
		writeErr(w, http.StatusConflict, err.Error())
	}
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// count returns the count for one status (missing key -> 0) for the cards.
func count(m map[string]int, st string) int { return m[st] }

// add sums two counts (dashboard "Active" = downloading + processing).
func add(a, b int) int { return a + b }

// total sums all counts (dashboard "Total"; List is capped at 200 rows).
func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// badge maps a task status to a daisyUI badge class.
func badge(st string) string {
	switch st {
	case "done":
		return "badge-success"
	case "failed":
		return "badge-error"
	case "downloading", "processing":
		return "badge-info"
	case "paused":
		return "badge-ghost"
	default:
		return "badge-warning"
	}
}

// ftime renders a timestamp compactly for the table.
func ftime(t time.Time) string { return t.Local().Format("15:04 02-Jan-06") }

// recName is the recording's own name (its output folder, e.g.
// "Class 200_0"): the second-to-last segment of the processed video path,
// falling back to the URL-extracted recording ID before the task is processed.
// Mirrors the page's client-side recName so the SSR snapshot and the React
// table agree.
func recName(t Task) string {
	var video string
	if t.ReportJSON != "" {
		var rep struct {
			Video string `json:"video"`
		}
		if json.Unmarshal([]byte(t.ReportJSON), &rep) == nil {
			video = rep.Video
		}
	}
	if video == "" && strings.HasPrefix(t.Stage, "done:") {
		video = strings.TrimSpace(strings.TrimPrefix(t.Stage, "done:"))
	}
	if video != "" {
		parts := strings.FieldsFunc(video, func(r rune) bool { return r == '/' || r == '\\' })
		if len(parts) >= 2 {
			return parts[len(parts)-2]
		}
	}
	if t.RecordingID != "" {
		return t.RecordingID
	}
	return t.ID
}

var dashTmpl = template.Must(template.New("dash").Funcs(template.FuncMap{
	"short": shortID,
	"badge": badge,
	"count": count,
	"add":   add,
	"total": total,
	"ftime": ftime,
}).Parse(dashFragment))

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"short":   shortID,
	"badge":   badge,
	"count":   count,
	"add":     add,
	"total":   total,
	"ftime":   ftime,
	"recName": recName,
}).Parse(pageShell))

// dashFragment is the live region polled by htmx every 3s: status cards,
// filter buttons and the task table. Projekktor-free, no page reloads.
const dashFragment = `
<div class="grid grid-cols-2 gap-2 sm:grid-cols-5" id="cards">
  <div class="stat rounded-box bg-base-200 px-4 py-2 shadow"><div class="stat-title text-xs">Queued</div><div class="stat-value text-2xl text-warning">{{count .Counts "queued"}}</div></div>
  <div class="stat rounded-box bg-base-200 px-4 py-2 shadow"><div class="stat-title text-xs">Active</div><div class="stat-value text-2xl text-info">{{add (count .Counts "downloading") (count .Counts "processing")}}</div></div>
  <div class="stat rounded-box bg-base-200 px-4 py-2 shadow"><div class="stat-title text-xs">Done</div><div class="stat-value text-2xl text-success">{{count .Counts "done"}}</div></div>
  <div class="stat rounded-box bg-base-200 px-4 py-2 shadow"><div class="stat-title text-xs">Failed</div><div class="stat-value text-2xl text-error">{{count .Counts "failed"}}</div></div>
  <div class="stat rounded-box bg-base-200 px-4 py-2 shadow"><div class="stat-title text-xs">Total</div><div class="stat-value text-2xl">{{total .Counts}}</div></div>
</div>
<div class="mt-3 flex flex-wrap gap-2" role="group" aria-label="Status filter">
  <button class="btn btn-xs filter-btn btn-active" data-filter="all">All</button>
  <button class="btn btn-xs filter-btn" data-filter="queued">Queued</button>
  <button class="btn btn-xs filter-btn" data-filter="active">Active</button>
  <button class="btn btn-xs filter-btn" data-filter="done">Done</button>
  <button class="btn btn-xs filter-btn" data-filter="failed">Failed</button>
  <span class="ml-auto self-center text-xs opacity-60" id="shown"></span>
</div>
<div class="mt-3 overflow-x-auto rounded-box border border-base-300 shadow">
<table class="table table-zebra table-sm" id="tasks">
<thead><tr>
<th><button class="sort-btn link link-hover" data-sort="rec">Recording (sort)</button></th>
<th>Status</th><th>Att.</th><th>State / Detail</th>
<th><button class="sort-btn link link-hover" data-sort="created">Created (sort)</button></th>
<th>Updated</th><th>Link</th>
</tr></thead>
<tbody>
{{range .Tasks}}<tr data-status="{{.Status}}" data-rec="{{.RecordingID}}" data-created="{{.CreatedAt.Unix}}">
<td class="max-w-44 truncate font-mono text-xs" title="{{.ID}}">{{.RecordingID}}</td>
<td><span class="badge badge-sm {{badge .Status}}">{{.Status}}</span></td>
<td class="text-center">{{.Attempts}}</td>
<td class="max-w-72 truncate text-xs opacity-80" title="{{if .LastError}}{{.LastError}}{{else if .Stage}}{{.Stage}}{{end}}">{{if .LastError}}<span class="text-error">{{.LastError}}</span>{{else if .Stage}}<span class="font-mono text-info">{{.Stage}}</span>{{else if .ReportJSON}}<details><summary class="link">report</summary><pre class="whitespace-pre-wrap text-xs">{{.ReportJSON}}</pre></details>{{else}}--{{end}}</td>
<td class="whitespace-nowrap text-xs" title="{{.CreatedAt}}">{{ftime .CreatedAt}}</td>
<td class="whitespace-nowrap text-xs" title="{{.UpdatedAt}}">{{ftime .UpdatedAt}}</td>
<td><a class="link link-primary text-xs" href="{{.URL}}" target="_blank" rel="noopener">open</a></td>
</tr>{{else}}<tr><td colspan="7" class="py-6 text-center opacity-60">No tasks yet - paste a recording URL above.</td></tr>{{end}}
</tbody>
</table>
</div>`

//go:embed index.html
var pageShell string
