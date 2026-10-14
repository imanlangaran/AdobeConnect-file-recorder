package server

import (
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
	for _, want := range []string{"<table", "p15rdeovw4ge", "<form", `name="url"`, "htmx", "tailwindcss", "daisyui"} {
		if !strings.Contains(body, want) {
			t.Fatalf("status page missing %q: %.200s", want, body)
		}
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatal("status page must not meta-refresh (htmx polls instead)")
	}
}
