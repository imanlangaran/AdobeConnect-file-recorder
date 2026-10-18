package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu    sync.Mutex
	tasks []Task
	byURL map[string]Task
	byID  map[string]Task
}

func newFake() *fakeStore {
	return &fakeStore{byURL: map[string]Task{}, byID: map[string]Task{}}
}

func (f *fakeStore) Enqueue(url, recordingID string) (Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.byURL[url]; ok {
		return t, true, nil
	}
	t := Task{ID: "task-" + strings.ReplaceAll(recordingID, "/", "-"), URL: url,
		RecordingID: recordingID, Status: "queued", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.tasks = append(f.tasks, t)
	f.byURL[url] = t
	f.byID[t.ID] = t
	return t, false, nil
}

func (f *fakeStore) Get(id string) (Task, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	return t, ok
}

func (f *fakeStore) List(limit int) []Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 || limit > len(f.tasks) {
		limit = len(f.tasks)
	}
	out := make([]Task, limit)
	copy(out, f.tasks[:limit])
	return out
}

func (f *fakeStore) Counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]int{}
	for _, t := range f.tasks {
		m[t.Status]++
	}
	return m
}

func (f *fakeStore) Delete(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := -1
	for i, t := range f.tasks {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}
	delete(f.byID, id)
	delete(f.byURL, f.tasks[idx].URL)
	f.tasks = append(f.tasks[:idx], f.tasks[idx+1:]...)
	return true
}

func (f *fakeStore) Retry(id, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok {
		return ErrNotFound
	}
	switch t.Status {
	case "failed", "paused", "done":
	default:
		return ErrBadState
	}
	t.Status = "queued"
	t.SkipDownload = mode == "reconstruct"
	t.LastError = ""
	t.UpdatedAt = time.Now()
	f.byID[id] = t
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			f.tasks[i] = t
		}
	}
	return nil
}

func (f *fakeStore) Stop(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok || (t.Status != "downloading" && t.Status != "processing") {
		return false
	}
	t.Status = "paused"
	f.byID[id] = t
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			f.tasks[i] = t
		}
	}
	return true
}

func TestPostValidAccepted(t *testing.T) {
	h := New(newFake())
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"url":"https://example.adobeconnect.com/p15rdeovw4ge/"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var got Task
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RecordingID != "p15rdeovw4ge" {
		t.Fatalf("want recordingID p15rdeovw4ge, got %q", got.RecordingID)
	}
}

func TestPostInvalidBadRequest(t *testing.T) {
	h := New(newFake())
	for _, body := range []string{`{"url":""}`, `{}`, `{"url":"not a url!!!"}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: want 400, got %d", body, rec.Code)
		}
	}
}

func TestPostFormAccepted(t *testing.T) {
	h := New(newFake())
	form := strings.NewReader("url=" + "https%3A%2F%2Fexample.adobeconnect.com%2Fp15rdeovw4ge%2F")
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "p15rdeovw4ge") || !strings.Contains(body, "alert-success") {
		t.Fatalf("form response missing success alert/task: %.200s", body)
	}
}

func TestPostFormInvalidShowsAlert(t *testing.T) {
	h := New(newFake())
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader("url=not+a+url"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// HTML fragment (htmx swaps 2xx only), error-styled - API clients still get 400.
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 fragment, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "alert-error") {
		t.Fatalf("form error missing alert-error: %.200s", body)
	}
}

func TestDashboardPartial(t *testing.T) {
	st := newFake()
	if _, _, err := st.Enqueue("https://example.adobeconnect.com/p15rdeovw4ge/", "p15rdeovw4ge"); err != nil {
		t.Fatal(err)
	}
	h := New(st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/partials/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<table") || !strings.Contains(body, "p15rdeovw4ge") {
		t.Fatalf("partial missing table/task: %.200s", body)
	}
}

func TestStatusPageHasTable(t *testing.T) {
	st := newFake()
	if _, _, err := st.Enqueue("https://example.adobeconnect.com/p15rdeovw4ge/", "p15rdeovw4ge"); err != nil {
		t.Fatal(err)
	}
	h := New(st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<table", "p15rdeovw4ge", "<form", `name="url"`, "react", "tailwindcss", "daisyui"} {
		if !strings.Contains(body, want) {
			t.Fatalf("status page missing %q: %.200s", want, body)
		}
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatal("status page must not meta-refresh")
	}
}

func TestDeleteTask(t *testing.T) {
	st := newFake()
	task, _, err := st.Enqueue("https://example.adobeconnect.com/p15rdeovw4ge/", "p15rdeovw4ge")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)

	// Delete existing task
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/tasks/"+task.ID, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res["ok"] != true {
		t.Fatalf("expected ok=true, got %+v", res)
	}

	// Delete missing task returns 404
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodDelete, "/api/tasks/missing-id", nil)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec2.Code)
	}
}

func TestSSEEvents(t *testing.T) {
	st := newFake()
	h := New(st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for ping
	time.Sleep(50 * time.Millisecond)

	// Trigger enqueue to generate update event
	postReq := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"url":"https://example.adobeconnect.com/p123/"}`))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	h.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("enqueue code: %d", postRec.Code)
	}

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: ping") {
		t.Fatalf("SSE missing ping: %s", body)
	}
	if !strings.Contains(body, "event: update") {
		t.Fatalf("SSE missing update: %s", body)
	}
}

func TestDashboardShowsStage(t *testing.T) {
	st := newFake()
	task, _, _ := st.Enqueue("https://example.adobeconnect.com/p15rdeovw4ge/", "p15rdeovw4ge")
	st.tasks[0].Stage = "reconstructing 42%"
	st.byID[task.ID] = st.tasks[0]
	h := New(st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/partials/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reconstructing 42%") {
		t.Fatalf("dashboard missing stage: %s", rec.Body.String())
	}
}

func TestRetryEndpoints(t *testing.T) {
	st := newFake()
	task, _, _ := st.Enqueue("https://example.adobeconnect.com/p1/", "p1")
	h := New(st)
	post := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, url, nil))
		return rec
	}

	// queued task is not retryable -> 409
	if rec := post("/api/tasks/" + task.ID + "/retry?mode=full"); rec.Code != http.StatusConflict {
		t.Fatalf("retry queued: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	// unknown mode -> 400
	if rec := post("/api/tasks/" + task.ID + "/retry?mode=nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: want 400, got %d", rec.Code)
	}
	// unknown task -> 404
	if rec := post("/api/tasks/missing/retry?mode=full"); rec.Code != http.StatusNotFound {
		t.Fatalf("retry missing: want 404, got %d", rec.Code)
	}
	// failed -> queued, reconstruct-only sets skipDownload
	st.byID[task.ID] = Task{ID: task.ID, Status: "failed"}
	st.tasks = []Task{st.byID[task.ID]}
	if rec := post("/api/tasks/" + task.ID + "/retry?mode=reconstruct"); rec.Code != http.StatusOK {
		t.Fatalf("retry failed: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.Get(task.ID)
	if got.Status != "queued" || !got.SkipDownload {
		t.Fatalf("after reconstruct retry: %+v", got)
	}
}

func TestStopEndpoint(t *testing.T) {
	st := newFake()
	task, _, _ := st.Enqueue("https://example.adobeconnect.com/p1/", "p1")
	h := New(st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/stop", nil))
	if rec.Code != http.StatusConflict { // queued: nothing running
		t.Fatalf("stop queued: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	st.byID[task.ID] = Task{ID: task.ID, Status: "downloading"}
	st.tasks = []Task{st.byID[task.ID]}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/stop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop active: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := st.Get(task.ID); got.Status != "paused" {
		t.Fatalf("after stop: %+v", got)
	}
}
