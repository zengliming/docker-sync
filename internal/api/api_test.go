package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"docker-sync/internal/model"
	"docker-sync/internal/scheduler"
	"docker-sync/internal/store"
)

// fakeScheduler is a SchedulerAPI double recording the calls the API makes.
type fakeScheduler struct {
	mu sync.Mutex

	run       model.Run
	syncErr   error
	forceSeen []bool

	check    scheduler.CheckResult
	checkErr error

	changed   []string
	deleted   []string
	settings  int
	running   int
	started   bool
	syncCalls int
}

func (f *fakeScheduler) TriggerSync(taskID, trigger string, force bool) (model.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	f.forceSeen = append(f.forceSeen, force)
	if f.syncErr != nil {
		return model.Run{}, f.syncErr
	}
	run := f.run
	run.TaskID = taskID
	run.Trigger = trigger
	return run, nil
}

func (f *fakeScheduler) CheckNow(_ context.Context, _ string) (scheduler.CheckResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.check, f.checkErr
}

func (f *fakeScheduler) TaskChanged(taskID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changed = append(f.changed, taskID)
}

func (f *fakeScheduler) TaskDeleted(taskID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, taskID)
}

func (f *fakeScheduler) SettingsChanged() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings++
}

func (f *fakeScheduler) RunningCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeScheduler) Started() bool { return f.started }

type testEnv struct {
	handler http.Handler
	store   *store.Store
	sched   *fakeScheduler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Options{Dir: dir, Logger: nil})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	fake := &fakeScheduler{started: true}
	web := fstest.MapFS{
		"index.html":     {Data: []byte("<!doctype html><title>index</title>")},
		"static/app.js":  {Data: []byte("console.log('app');")},
		"static/app.css": {Data: []byte("body{margin:0}")},
	}
	srv, err := New(Options{Store: st, Scheduler: fake, Version: "test-version", WebUI: web})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	t.Cleanup(srv.Close)
	return &testEnv{handler: srv.Handler(), store: st, sched: fake}
}

func (e *testEnv) do(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

type taskEnvelope struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Image          string `json:"image"`
	Platform       string `json:"platform"`
	ScheduleType   string `json:"scheduleType"`
	IntervalMinute int    `json:"intervalMinutes"`
	Cron           string `json:"cron"`
	AutoSync       bool   `json:"autoSync"`
	KeepVersions   int    `json:"keepVersions"`
	Enabled        bool   `json:"enabled"`
	Status         string `json:"status"`
	LastError      string `json:"lastError"`
}

func TestCreateTaskAppliesDefaults(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeBody[taskEnvelope](t, rec)
	if got.ID == "" || !strings.HasPrefix(got.ID, "t_") {
		t.Fatalf("task id = %q", got.ID)
	}
	if got.Image != "docker.io/library/alpine:3.20" {
		t.Fatalf("image = %q", got.Image)
	}
	if got.Platform != "linux/amd64" {
		t.Fatalf("platform = %q", got.Platform)
	}
	if got.ScheduleType != "interval" || got.IntervalMinute != 60 || got.KeepVersions != 3 {
		t.Fatalf("defaults = %+v", got)
	}
	if got.Name != got.Image {
		t.Fatalf("name = %q, want image", got.Name)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct {
		name string
		body string
	}{
		{"missing image", `{"name":"x"}`},
		{"bad image", `{"image":"not a valid ref!!"}`},
		{"bad platform", `{"image":"alpine:3.20","platform":"linux/amd64/extra/bits"}`},
		{"cron without spec", `{"image":"alpine:3.20","scheduleType":"cron"}`},
		{"bad cron", `{"image":"alpine:3.20","scheduleType":"cron","cron":"not a cron"}`},
		{"unknown schedule type", `{"image":"alpine:3.20","scheduleType":"weekly"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, http.MethodPost, "/api/tasks", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			var errBody map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody["error"] == "" {
				t.Fatalf("body = %q", rec.Body.String())
			}
		})
	}
}

func TestTaskReadUpdateDelete(t *testing.T) {
	env := newTestEnv(t)
	created := decodeBody[taskEnvelope](t, env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20","enabled":true}`))

	rec := env.do(t, http.MethodGet, "/api/tasks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if got := decodeBody[taskEnvelope](t, rec); got.ID != created.ID || got.Status != "idle" {
		t.Fatalf("get = %+v", got)
	}

	list := decodeBody[struct {
		Tasks []taskEnvelope `json:"tasks"`
	}](t, env.do(t, http.MethodGet, "/api/tasks", ""))
	if len(list.Tasks) != 1 {
		t.Fatalf("tasks = %d", len(list.Tasks))
	}

	updated := decodeBody[taskEnvelope](t, env.do(t, http.MethodPut, "/api/tasks/"+created.ID,
		`{"image":"ghcr.io/owner/app:v1","name":"app","scheduleType":"interval","intervalMinutes":5,"autoSync":true,"keepVersions":0,"enabled":true}`))
	if updated.Image != "ghcr.io/owner/app:v1" || updated.IntervalMinute != 5 || updated.KeepVersions != 3 {
		t.Fatalf("updated = %+v", updated)
	}
	// Creation schedules the task once and the update reschedules it.
	if len(env.sched.changed) != 2 || env.sched.changed[0] != created.ID || env.sched.changed[1] != created.ID {
		t.Fatalf("TaskChanged calls = %+v", env.sched.changed)
	}

	if rec := env.do(t, http.MethodPut, "/api/tasks/t_missing", `{"image":"alpine:3.20"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("update missing status = %d", rec.Code)
	}

	rec = env.do(t, http.MethodDelete, "/api/tasks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if got := decodeBody[map[string]bool](t, rec); !got["ok"] {
		t.Fatalf("delete body = %v", got)
	}
	if len(env.sched.deleted) != 1 || env.sched.deleted[0] != created.ID {
		t.Fatalf("TaskDeleted calls = %+v", env.sched.deleted)
	}
	if rec := env.do(t, http.MethodGet, "/api/tasks/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted status = %d", rec.Code)
	}
	if rec := env.do(t, http.MethodDelete, "/api/tasks/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing status = %d", rec.Code)
	}
}

func TestSyncTask(t *testing.T) {
	env := newTestEnv(t)
	created := decodeBody[taskEnvelope](t, env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20"}`))
	env.sched.run = model.Run{ID: "r_test0001", Status: model.RunStatusRunning, Phase: model.PhaseQueued}

	rec := env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/sync", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Run model.Run `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Run.ID != "r_test0001" || payload.Run.TaskID != created.ID || payload.Run.Trigger != model.TriggerManual {
		t.Fatalf("run = %+v", payload.Run)
	}
	if len(env.sched.forceSeen) != 1 || env.sched.forceSeen[0] {
		t.Fatalf("force calls = %+v", env.sched.forceSeen)
	}

	if rec := env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/sync", `{"force":true}`); rec.Code != http.StatusAccepted {
		t.Fatalf("force status = %d", rec.Code)
	}
	if len(env.sched.forceSeen) != 2 || !env.sched.forceSeen[1] {
		t.Fatalf("force calls = %+v", env.sched.forceSeen)
	}

	env.sched.syncErr = scheduler.ErrBusy
	if rec := env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/sync", ""); rec.Code != http.StatusConflict {
		t.Fatalf("busy status = %d", rec.Code)
	}
	env.sched.syncErr = scheduler.ErrNotFound
	if rec := env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/sync", `{"force":"yes"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d", rec.Code)
	}
}

func TestCheckTask(t *testing.T) {
	env := newTestEnv(t)
	created := decodeBody[taskEnvelope](t, env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20"}`))
	env.sched.check = scheduler.CheckResult{
		UpdateAvailable: true,
		RemoteDigest:    "sha256:remote",
		CurrentDigest:   "sha256:current",
		Image:           "docker.io/library/alpine:3.20",
		Platform:        "linux/amd64",
	}

	rec := env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/check", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["updateAvailable"] != true || payload["remoteDigest"] != "sha256:remote" ||
		payload["currentDigest"] != "sha256:current" || payload["platform"] != "linux/amd64" {
		t.Fatalf("payload = %v", payload)
	}

	env.sched.checkErr = errors.New("registry: unauthorized")
	rec = env.do(t, http.MethodPost, "/api/tasks/"+created.ID+"/check", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failure status = %d, body %s", rec.Code, rec.Body.String())
	}

	env.sched.checkErr = scheduler.ErrNotFound
	if rec := env.do(t, http.MethodPost, "/api/tasks/t_missing/check", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", rec.Code)
	}
}

func TestRunsEndpoints(t *testing.T) {
	env := newTestEnv(t)
	task := decodeBody[taskEnvelope](t, env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20"}`))

	if rec := env.do(t, http.MethodGet, "/api/tasks/t_missing/runs", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing task runs status = %d", rec.Code)
	}

	run, err := env.store.CreateRun(model.Run{
		TaskID:   task.ID,
		TaskName: task.Name,
		Image:    task.Image,
		Platform: task.Platform,
		Trigger:  model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	env.store.AppendRunLog(run.ID, model.LevelInfo, "第一行日志")
	env.store.AppendRunLog(run.ID, model.LevelInfo, "第二行日志")

	rec := env.do(t, http.MethodGet, "/api/tasks/"+task.ID+"/runs?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list struct {
		Runs []model.Run `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Runs) != 1 || list.Runs[0].ID != run.ID {
		t.Fatalf("runs = %+v", list.Runs)
	}
	if len(list.Runs[0].Logs) != 0 {
		t.Fatalf("list runs must not include logs, got %d", len(list.Runs[0].Logs))
	}

	detail := decodeBody[model.Run](t, env.do(t, http.MethodGet, "/api/runs/"+run.ID, ""))
	if len(detail.Logs) != 2 || detail.Logs[0].Message != "第一行日志" {
		t.Fatalf("detail logs = %+v", detail.Logs)
	}
	if rec := env.do(t, http.MethodGet, "/api/runs/r_missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing run status = %d", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/api/runs?limit=1000", ""); rec.Code != http.StatusOK {
		t.Fatalf("global runs status = %d", rec.Code)
	}
}

func TestFilesEndpoints(t *testing.T) {
	env := newTestEnv(t)
	task := decodeBody[taskEnvelope](t, env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20"}`))

	const name = "library_alpine_3.20_linux_amd64_abcdef123456.tar"
	content := []byte("0123456789-tar-content")
	if err := os.WriteFile(env.store.TarPath(name), content, 0o644); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	env.store.PutTarFile(store.TarFile{
		Name:     name,
		Size:     int64(len(content)),
		ModTime:  time.Now(),
		TaskID:   task.ID,
		TaskName: task.Name,
		Image:    task.Image,
		Digest:   "sha256:abcdef",
	})

	list := decodeBody[struct {
		Files []model.FileItem `json:"files"`
	}](t, env.do(t, http.MethodGet, "/api/files", ""))
	if len(list.Files) != 1 || list.Files[0].Name != name || list.Files[0].URL != "/api/files/"+name {
		t.Fatalf("files = %+v", list.Files)
	}
	if list.Files[0].TaskName != task.Name || list.Files[0].Digest != "sha256:abcdef" {
		t.Fatalf("file meta = %+v", list.Files[0])
	}

	rec := env.do(t, http.MethodGet, "/api/files/"+name, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-tar" {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, name) {
		t.Fatalf("content disposition = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("body = %q", rec.Body.String())
	}

	rangeReq := httptest.NewRequest(http.MethodGet, "/api/files/"+name, nil)
	rangeReq.Header.Set("Range", "bytes=0-3")
	rangeRec := httptest.NewRecorder()
	env.handler.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d", rangeRec.Code)
	}
	if rangeRec.Body.String() != "0123" {
		t.Fatalf("range body = %q", rangeRec.Body.String())
	}

	if rec := env.do(t, http.MethodGet, "/api/files/missing.tar", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing file status = %d", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/api/files/bad%20name.tar", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name status = %d", rec.Code)
	}
	if rec := env.do(t, http.MethodDelete, "/api/files/bad%20name.tar", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name delete status = %d", rec.Code)
	}

	if rec := env.do(t, http.MethodDelete, "/api/files/"+name, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	empty := decodeBody[struct {
		Files []model.FileItem `json:"files"`
	}](t, env.do(t, http.MethodGet, "/api/files", ""))
	if len(empty.Files) != 0 {
		t.Fatalf("files after delete = %+v", empty.Files)
	}
	if _, err := os.Stat(filepath.Join(env.store.TarsDir(), name)); !os.IsNotExist(err) {
		t.Fatalf("tar file still exists: %v", err)
	}
}

func TestSettingsEndpoints(t *testing.T) {
	env := newTestEnv(t)

	initial := decodeBody[model.Settings](t, env.do(t, http.MethodGet, "/api/settings", ""))
	if initial.DataDir == "" {
		t.Fatal("dataDir missing")
	}
	if initial.MaxConcurrentRuns == 0 {
		t.Fatalf("default settings = %+v", initial)
	}

	body := `{"defaultPlatform":"linux/arm64","maxConcurrentRuns":4,"maxConcurrentDownloads":2,"blobCacheMaxGB":10,"blobCacheMaxAgeDays":30,
		"registries":[{"host":"ghcr.io","username":"u","password":"p","hasPassword":true,"insecure":false}],"dataDir":"/ignored"}`
	rec := env.do(t, http.MethodPut, "/api/settings", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"password"`) {
		t.Fatalf("response leaks password: %s", rec.Body.String())
	}
	saved := decodeBody[model.Settings](t, rec)
	if saved.DefaultPlatform != "linux/arm64" || saved.MaxConcurrentRuns != 4 || saved.DataDir == "/ignored" {
		t.Fatalf("saved = %+v", saved)
	}
	if len(saved.Registries) != 1 || !saved.Registries[0].HasPassword || saved.Registries[0].Password != "" {
		t.Fatalf("registries = %+v", saved.Registries)
	}
	if env.sched.settings != 1 {
		t.Fatalf("SettingsChanged calls = %d", env.sched.settings)
	}
	if raw := env.store.SnapshotSettings(); raw.Registries[0].Password != "p" {
		t.Fatalf("password not stored: %+v", raw.Registries)
	}

	for _, bad := range []string{
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":0,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":99,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7}`,
		`{"defaultPlatform":"","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7}`,
		`{"defaultPlatform":"bogus","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":0,"blobCacheMaxAgeDays":7}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":0}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"registries":[{"host":""}]}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"registries":[{"host":"a.io"},{"host":"A.IO"}]}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"registries":[{"host":"https://a.io"}]}`,
	} {
		if rec := env.do(t, http.MethodPut, "/api/settings", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad settings status = %d for %s", rec.Code, bad)
		}
	}
}

func TestSettingsProxyFields(t *testing.T) {
	env := newTestEnv(t)

	body := `{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,
		"proxyUrl":" socks5://127.0.0.1:7890 ","noProxy":"docker.io, *.corp.local, 10.0.0.1:5000",
		"registries":[{"host":"ghcr.io","proxyUrl":"http://127.0.0.1:1080"},{"host":"registry.local:5000","proxyUrl":"-"}]}`
	rec := env.do(t, http.MethodPut, "/api/settings", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, body %s", rec.Code, rec.Body.String())
	}
	saved := decodeBody[model.Settings](t, rec)
	if saved.ProxyURL != "socks5://127.0.0.1:7890" {
		t.Fatalf("proxyUrl = %q, want trimmed value", saved.ProxyURL)
	}
	if saved.NoProxy != "docker.io, *.corp.local, 10.0.0.1:5000" {
		t.Fatalf("noProxy = %q", saved.NoProxy)
	}
	if len(saved.Registries) != 2 || saved.Registries[0].ProxyURL != "http://127.0.0.1:1080" || saved.Registries[1].ProxyURL != "-" {
		t.Fatalf("registries = %+v", saved.Registries)
	}
	got := decodeBody[model.Settings](t, env.do(t, http.MethodGet, "/api/settings", ""))
	if got.ProxyURL != "socks5://127.0.0.1:7890" || got.Registries[0].ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("reloaded settings = %+v", got)
	}
	// "-" means "always connect directly" and must be accepted globally.
	if rec := env.do(t, http.MethodPut, "/api/settings",
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"proxyUrl":"-","noProxy":""}`); rec.Code != http.StatusOK {
		t.Fatalf("dash proxy status = %d, body %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"proxyUrl":"ftp://127.0.0.1:21"}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"proxyUrl":"http://"}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"noProxy":"http://docker.io"}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"noProxy":"docker.io/v2"}`,
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"registries":[{"host":"ghcr.io","proxyUrl":"socks4a://127.0.0.1:1080"}]}`,
	} {
		if rec := env.do(t, http.MethodPut, "/api/settings", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad proxy settings status = %d for %s", rec.Code, bad)
		}
	}
}

func TestProxyTestEndpoint(t *testing.T) {
	env := newTestEnv(t)

	for _, bad := range []string{
		`{"proxyUrl":"ftp://127.0.0.1:21"}`,
		`{"image":"not a valid! ref"}`,
		`{"proxyUrl":"http://127.0.0.1:1","noProxy":"http://has-a-scheme"}`,
		`{"noProxy":"some/path"}`,
	} {
		if rec := env.do(t, http.MethodPost, "/api/settings/proxy-test", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad proxy-test status = %d for %s", rec.Code, bad)
		}
	}

	// An explicit, unreachable proxy must report a failure without failing the
	// HTTP call itself.
	rec := env.do(t, http.MethodPost, "/api/settings/proxy-test", `{"proxyUrl":"http://127.0.0.1:1","image":"alpine:3.20"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy-test status = %d, body %s", rec.Code, rec.Body.String())
	}
	out := decodeBody[proxyTestResponse](t, rec)
	if out.OK {
		t.Fatalf("probe through a dead proxy reported success: %+v", out)
	}
	if out.Target != "https://registry-1.docker.io/v2/" {
		t.Fatalf("target = %q", out.Target)
	}
	if out.ProxyURL != "http://127.0.0.1:1" || out.Error == "" {
		t.Fatalf("probe result = %+v", out)
	}

	// Omitting the fields probes with the stored configuration.
	if rec := env.do(t, http.MethodPut, "/api/settings",
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"proxyUrl":"http://127.0.0.1:1"}`); rec.Code != http.StatusOK {
		t.Fatalf("put settings status = %d", rec.Code)
	}
	out = decodeBody[proxyTestResponse](t, env.do(t, http.MethodPost, "/api/settings/proxy-test", `{}`))
	if out.OK || out.ProxyURL != "http://127.0.0.1:1" {
		t.Fatalf("stored-config probe = %+v", out)
	}

	// A registry marked as insecure is probed over plain HTTP.
	if rec := env.do(t, http.MethodPut, "/api/settings",
		`{"defaultPlatform":"linux/amd64","maxConcurrentRuns":2,"maxConcurrentDownloads":4,"blobCacheMaxGB":20,"blobCacheMaxAgeDays":7,"registries":[{"host":"registry.local:5000","insecure":true}]}`); rec.Code != http.StatusOK {
		t.Fatalf("put insecure settings status = %d", rec.Code)
	}
	out = decodeBody[proxyTestResponse](t, env.do(t, http.MethodPost, "/api/settings/proxy-test", `{"image":"registry.local:5000/team/app:1","proxyUrl":"http://127.0.0.1:1"}`))
	if out.Target != "http://registry.local:5000/v2/" {
		t.Fatalf("insecure target = %q", out.Target)
	}
}

func TestHealthAndOverview(t *testing.T) {
	env := newTestEnv(t)
	health := decodeBody[map[string]any](t, env.do(t, http.MethodGet, "/api/health", ""))
	if health["status"] != "ok" || health["version"] != "test-version" {
		t.Fatalf("health = %v", health)
	}
	if _, ok := health["uptimeSec"]; !ok {
		t.Fatalf("health missing uptimeSec: %v", health)
	}

	env.do(t, http.MethodPost, "/api/tasks", `{"image":"alpine:3.20","enabled":true}`)
	env.do(t, http.MethodPost, "/api/tasks", `{"image":"busybox:1.36"}`)
	overview := decodeBody[model.Overview](t, env.do(t, http.MethodGet, "/api/overview", ""))
	if overview.TaskCount != 2 || overview.EnabledCount != 1 {
		t.Fatalf("overview = %+v", overview)
	}
	if overview.RecentRuns == nil {
		t.Fatal("recentRuns must be a JSON array")
	}
}

func TestStaticAndNotFound(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(t, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index") {
		t.Fatalf("root = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache control = %q", got)
	}

	rec = env.do(t, http.MethodGet, "/static/app.js", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("static = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("static cache control = %q", got)
	}

	// Unknown non-API paths fall back to the SPA entry point.
	rec = env.do(t, http.MethodGet, "/some/route", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index") {
		t.Fatalf("spa fallback = %d %q", rec.Code, rec.Body.String())
	}

	rec = env.do(t, http.MethodGet, "/api/does-not-exist", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("api 404 = %d %q", rec.Code, rec.Body.String())
	}

	rec = env.do(t, http.MethodPost, "/", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post root status = %d", rec.Code)
	}
}

func TestEventsStreamBroadcasts(t *testing.T) {
	env := newTestEnv(t)
	srv := httptest.NewServer(env.handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events request: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(first, ":") {
		t.Fatalf("greeting = %q", first)
	}

	// Any store change is broadcast to connected clients as event: update.
	env.store.SetSettings(env.store.SnapshotSettings())

	deadline := time.Now().Add(3 * time.Second)
	var sawEvent bool
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "event: update") {
			sawEvent = true
			data, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(data, "data: {") {
				t.Fatalf("event data = %q (%v)", data, err)
			}
			break
		}
	}
	if !sawEvent {
		t.Fatal("did not receive an update event")
	}
}

func TestMiddlewareGzipJSONOnly(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content encoding = %q", got)
	}

	// SSE must never be compressed.
	req = httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	env.handler.ServeHTTP(rec, req.WithContext(ctx))
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("sse content encoding = %q", got)
	}
}
