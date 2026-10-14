// Package server exposes the queue over HTTP with a status page.
// The page is a Tailwind/daisyUI/htmx UI served from static CDNs (no Go
// dependencies); Store/Task are local mirrors of the queue API, bridged in
// production by cli.queueAdapter.
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iman-langaran/AdobeConnect-file-recorder/internal/recordingurl"
)

// Task mirrors queue.Task.
type Task struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	RecordingID string    `json:"recordingID"`
	Status      string    `json:"status"`
	Stage       string    `json:"stage,omitempty"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"lastError"`
	ReportJSON  string    `json:"reportJSON,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Store is the subset of queue.Store the HTTP layer needs.
type Store interface {
	Enqueue(url, recordingID string) (t Task, existed bool, err error)
	Get(id string) (t Task, ok bool)
	List(limit int) []Task
	Counts() map[string]int
}

// New wires the routes and returns the handler.
func New(store Store) http.Handler {
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
		code := http.StatusAccepted
		if existed {
			code = http.StatusOK
		}
		respondTaskOK(w, r, code, t, existed)
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
	default:
		return "badge-warning"
	}
}

// ftime renders a timestamp compactly for the table.
func ftime(t time.Time) string { return t.Local().Format("15:04 02-Jan-06") }

var dashTmpl = template.Must(template.New("dash").Funcs(template.FuncMap{
	"short": shortID,
	"badge": badge,
	"count": count,
	"add":   add,
	"total": total,
	"ftime": ftime,
}).Parse(dashFragment))

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"short": shortID,
	"badge": badge,
	"count": count,
	"add":   add,
	"total": total,
	"ftime": ftime,
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

// pageShell is the full page: CDN libs, add-form, and the htmx live region.
const pageShell = `<!doctype html>
<html lang="en" data-theme="light"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ac-rec queue</title>
<script src="https://cdn.tailwindcss.com"></script>
<link href="https://cdn.jsdelivr.net/npm/daisyui@4.12.10/dist/full.min.css" rel="stylesheet">
<script src="https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js"></script>
</head><body class="min-h-screen bg-base-100">
<div class="mx-auto max-w-6xl px-4 py-6">
<header class="mb-4 flex flex-wrap items-baseline gap-3">
<h1 class="text-2xl font-bold">ac-rec queue</h1>
<a class="link link-primary text-sm" href="/api/tasks">JSON API</a>
<span class="badge badge-ghost badge-sm" id="health">checking...</span>
</header>
<section class="card bg-base-200 shadow">
<div class="card-body gap-2 p-4">
<h2 class="card-title text-base">Enqueue recording</h2>
<form id="add-form" hx-post="/api/tasks" hx-target="#form-msg" hx-swap="innerHTML"
  class="join w-full">
<input name="url" type="url" required placeholder="https://host/pXXXXXXXX/"
  class="input join-item input-bordered input-sm w-full" autocomplete="off">
<button class="btn btn-primary btn-sm join-item" type="submit">Add</button>
</form>
<div id="form-msg" aria-live="polite"></div>
<p class="text-xs opacity-60">Duplicates return the existing task instead of re-adding.</p>
</div>
</section>
<section class="mt-4" id="live" hx-get="/partials/dashboard" hx-trigger="load, every 3s, refresh" hx-swap="innerHTML">
` + dashFragment + `
</section>
<footer class="mt-6 text-center text-xs opacity-50">ac-rec queue - auto-refresh 3s - showing up to 200 newest</footer>
</div>
<script>
// Health dot (fetch API): green/red badge, polled with the dashboard.
async function health(){try{const r=await fetch('/healthz');const h=document.getElementById('health');if(!h)return;
h.textContent=r.ok?'online':'down';h.className='badge badge-sm '+(r.ok?'badge-success':'badge-error');}catch(e){}}
health();setInterval(health,10000);
// Client state (filter + sort) survives htmx swaps via re-apply on load.
let curFilter='all',curSort=null,sortDir=1;
function applyFilter(){let n=0;document.querySelectorAll('#tasks tbody tr[data-status]').forEach(tr=>{
const st=tr.dataset.status,show=curFilter==='all'||st===curFilter||(curFilter==='active'&&(st==='downloading'||st==='processing'));
tr.style.display=show?'':'none';if(show)n++;});
document.getElementById('shown').textContent=n+' shown';}
function applySort(){if(!curSort)return;const tb=document.querySelector('#tasks tbody');if(!tb)return;
[...tb.rows].filter(r=>r.dataset.status).sort((a,b)=>{const x=a.dataset[curSort]||'',y=b.dataset[curSort]||'';
return (x<y?-1:x>y?1:0)*sortDir;}).forEach(r=>tb.appendChild(r));}
document.addEventListener('click',e=>{
const f=e.target.closest('.filter-btn');if(f){curFilter=f.dataset.filter;
document.querySelectorAll('.filter-btn').forEach(b=>b.classList.remove('btn-active'));f.classList.add('btn-active');applyFilter();return;}
const s=e.target.closest('.sort-btn');if(s){const k=s.dataset.sort;
if(curSort===k){sortDir*=-1;}else{curSort=k;sortDir=1;}applySort();}});
function syncFilterUI(){document.querySelectorAll('.filter-btn').forEach(b=>b.classList.toggle('btn-active',b.dataset.filter===curFilter));}
document.body.addEventListener('htmx:afterSwap',e=>{if(e.target.id==='live'){syncFilterUI();applyFilter();applySort();}});
document.body.addEventListener('htmx:afterRequest',e=>{if(e.target.id!=='add-form')return;
if(e.detail.successful&&e.detail.xhr.responseText.indexOf('alert-error')===-1){e.target.reset();htmx.trigger('#live','refresh');}});
syncFilterUI();applyFilter();
</script>
</body></html>`
