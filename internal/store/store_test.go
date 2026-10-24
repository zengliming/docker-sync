package store

import (
	"os"
	"path/filepath"
	"testing"

	"docker-sync/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTaskCRUDRoundTrip(t *testing.T) {
	s := openTestStore(t)

	created, err := s.CreateTask(model.Task{
		Name:            "alpine 3.20",
		Image:           "alpine:3.20",
		Platform:        "linux/amd64",
		ScheduleType:    model.ScheduleInterval,
		IntervalMinutes: 60,
		KeepVersions:    3,
		Enabled:         true,
		AutoSync:        true,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.ID == "" || created.CreatedAt.IsZero() {
		t.Fatalf("unexpected task: %+v", created)
	}

	got, ok := s.GetTask(created.ID)
	if !ok {
		t.Fatalf("GetTask(%s) not found", created.ID)
	}
	if got.Name != "alpine 3.20" || got.Image != "alpine:3.20" {
		t.Fatalf("unexpected stored task: %+v", got)
	}
	if got.Status != model.TaskStatusIdle {
		t.Fatalf("status = %q, want idle", got.Status)
	}

	if _, ok := s.GetTask("t_missing"); ok {
		t.Fatal("GetTask(missing) must fail")
	}

	patched, ok := s.PatchTask(created.ID, func(tk *model.Task) {
		tk.LastDigest = "sha256:abc"
		tk.Status = model.TaskStatusRunning
	})
	if !ok || patched.LastDigest != "sha256:abc" {
		t.Fatalf("PatchTask: %+v ok=%v", patched, ok)
	}

	list := s.ListTasks()
	if len(list) != 1 {
		t.Fatalf("ListTasks len = %d, want 1", len(list))
	}

	if !s.DeleteTask(created.ID) {
		t.Fatal("DeleteTask failed")
	}
	if s.DeleteTask(created.ID) {
		t.Fatal("second DeleteTask must fail")
	}
	if len(s.ListTasks()) != 0 {
		t.Fatal("task list must be empty")
	}
}

func TestRunLogRingAndHistory(t *testing.T) {
	s := openTestStore(t)

	task, err := s.CreateTask(model.Task{Name: "t", Image: "alpine:3.20"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := s.CreateRun(model.Run{TaskID: task.ID, TaskName: task.Name, Trigger: model.TriggerManual})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.Status != model.RunStatusRunning || run.Phase != model.PhaseQueued {
		t.Fatalf("unexpected run defaults: %+v", run)
	}

	for i := 0; i < MaxRunLogs+50; i++ {
		if !s.AppendRunLog(run.ID, model.LevelInfo, "line") {
			t.Fatalf("AppendRunLog failed at %d", i)
		}
	}
	stored, ok := s.GetRun(run.ID)
	if !ok {
		t.Fatal("GetRun not found")
	}
	if len(stored.Logs) != MaxRunLogs {
		t.Fatalf("logs = %d, want %d", len(stored.Logs), MaxRunLogs)
	}

	if _, ok := s.PatchRun(run.ID, func(r *model.Run) {
		r.Status = model.RunStatusSuccess
		r.Phase = model.PhaseDone
	}); !ok {
		t.Fatal("PatchRun failed")
	}
	// Listed runs carry no logs, as the API contract requires.
	list := s.ListRuns(10, "")
	if len(list) != 1 || list[0].Logs != nil {
		t.Fatalf("ListRuns = %+v", list)
	}
	if byTask := s.ListRuns(10, task.ID); len(byTask) != 1 {
		t.Fatalf("ListRuns(task) = %d runs", len(byTask))
	}
	if none := s.ListRuns(10, "t_other"); len(none) != 0 {
		t.Fatalf("ListRuns(other) = %d runs", len(none))
	}
	if _, ok := s.GetRun("r_missing"); ok {
		t.Fatal("GetRun(missing) must fail")
	}
}

func TestSettingsPasswordPreservation(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.SetSettings(model.Settings{
		DefaultPlatform:   "linux/amd64",
		MaxConcurrentRuns: 2,
		Registries: []model.RegistryAuth{
			{Host: "docker.io", Username: "me", Password: "secret"},
			{Host: "registry.local:5000", Username: "a", Password: "p2", Insecure: true},
		},
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	// GET must never leak the password.
	pub := s.GetSettings()
	for _, r := range pub.Registries {
		if r.Password != "" {
			t.Fatalf("password leaked for %s", r.Host)
		}
	}
	// An update that omits passwords keeps them.
	update := model.Settings{
		DefaultPlatform:        "linux/amd64",
		MaxConcurrentRuns:      4,
		MaxConcurrentDownloads: 2,
		BlobCacheMaxGB:         10,
		BlobCacheMaxAgeDays:    3,
		Registries: []model.RegistryAuth{
			{Host: "docker.io", Username: "me", HasPassword: true},
			{Host: "registry.local:5000", Username: "a", HasPassword: true, Insecure: true},
		},
	}
	got, err := s.SetSettings(update)
	if err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if got.MaxConcurrentRuns != 4 || got.MaxConcurrentDownloads != 2 {
		t.Fatalf("settings not updated: %+v", got)
	}
	auth, ok := s.RegistryAuthFor("docker.io")
	if !ok || auth.Password != "secret" {
		t.Fatalf("password not preserved: %+v ok=%v", auth, ok)
	}
	// Clearing the password works too.
	if _, err := s.SetSettings(model.Settings{
		MaxConcurrentRuns: 2,
		Registries:        []model.RegistryAuth{{Host: "docker.io", Username: "me"}},
	}); err != nil {
		t.Fatalf("SetSettings clear: %v", err)
	}
	auth, ok = s.RegistryAuthFor("docker.io")
	if !ok || auth.Password != "" {
		t.Fatalf("password not cleared: %+v ok=%v", auth, ok)
	}
	// docker.io and index.docker.io are interchangeable.
	if _, err := s.SetSettings(model.Settings{
		MaxConcurrentRuns: 2,
		Registries:        []model.RegistryAuth{{Host: "docker.io", Username: "u", Password: "p"}},
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if _, ok := s.RegistryAuthFor("index.docker.io"); !ok {
		t.Fatal("index.docker.io must match docker.io")
	}
	if _, ok := s.RegistryAuthFor("ghcr.io"); ok {
		t.Fatal("ghcr.io must not have credentials")
	}
}

func TestTarIndexAndPruning(t *testing.T) {
	s := openTestStore(t)

	if !ValidTarName("library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar") {
		t.Fatal("valid name rejected")
	}
	for _, bad := range []string{"../state.json", "a/b.tar", "", "..", "x\\y.tar"} {
		if ValidTarName(bad) {
			t.Fatalf("invalid name %q accepted", bad)
		}
	}

	task, err := s.CreateTask(model.Task{Name: "t", Image: "alpine:3.20"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	names := []string{"a.tar", "b.tar", "c.tar"}
	for i, n := range names {
		if err := writeTarFile(t, s.TarPath(n), int64(i+1)); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
		s.PutTarFile(TarFile{Name: n, Size: int64(i + 1), TaskID: task.ID, ModTime: timeNowPlus(i)})
	}

	items := s.ListFiles()
	if len(items) != 3 {
		t.Fatalf("ListFiles len = %d, want 3", len(items))
	}
	if items[0].Name != "c.tar" {
		t.Fatalf("newest first broken: %+v", items)
	}
	if items[0].URL != "/api/files/c.tar" {
		t.Fatalf("url = %q", items[0].URL)
	}

	removed := s.PruneTaskTars(task.ID, 1)
	if len(removed) != 2 {
		t.Fatalf("PruneTaskTars removed %v, want 2", removed)
	}
	if s.HasTar("a.tar") || s.HasTar("b.tar") {
		t.Fatal("old tars must be gone from disk")
	}
	if len(s.ListFiles()) != 1 {
		t.Fatalf("index len = %d, want 1", len(s.ListFiles()))
	}

	if err := s.DeleteTarFile("c.tar"); err != nil {
		t.Fatalf("DeleteTarFile: %v", err)
	}
	if s.HasTar("c.tar") {
		t.Fatal("file must be deleted")
	}
	if err := s.DeleteTarFile("c.tar"); err == nil {
		t.Fatal("deleting a missing file must fail")
	}
}

func TestBrokenStateFileDoesNotBlockStart(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "state.json", "{not json"); err != nil {
		t.Fatalf("write state: %v", err)
	}
	s, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open must survive a corrupt state file: %v", err)
	}
	defer s.Close()
	if len(s.ListTasks()) != 0 {
		t.Fatal("tasks must be empty")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.broken")); err != nil {
		t.Fatalf("broken state must be backed up: %v", err)
	}
}

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	task, err := s.CreateTask(model.Task{Name: "alpine", Image: "alpine:3.20", ScheduleType: model.ScheduleCron, Cron: "0 * * * *", KeepVersions: 4})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := s.CreateRun(model.Run{TaskID: task.ID, TaskName: task.Name, Trigger: model.TriggerCheck})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	s.AppendRunLog(run.ID, model.LevelInfo, "hello")
	if _, err := s.SetSettings(model.Settings{
		DefaultPlatform: "linux/amd64",
		Registries:      []model.RegistryAuth{{Host: "docker.io", Username: "u", Password: "p"}},
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.Close()

	s2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, ok := s2.GetTask(task.ID)
	if !ok {
		t.Fatal("task lost after reload")
	}
	if got.Cron != "0 * * * *" || got.KeepVersions != 4 {
		t.Fatalf("task fields lost: %+v", got)
	}
	loaded, ok := s2.GetRun(run.ID)
	if !ok || len(loaded.Logs) != 1 || loaded.Logs[0].Message != "hello" {
		t.Fatalf("run lost after reload: %+v ok=%v", loaded, ok)
	}
	auth, ok := s2.RegistryAuthFor("docker.io")
	if !ok || auth.Password != "p" {
		t.Fatalf("settings lost after reload: %+v ok=%v", auth, ok)
	}
}

func TestReconcileAdoptsUntrackedTars(t *testing.T) {
	dir := t.TempDir()
	if err := writeTarFile(t, filepath.Join(dir, "tars", "orphan.tar"), 7); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	s, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	items := s.ListFiles()
	if len(items) != 1 || items[0].Name != "orphan.tar" || items[0].Size != 7 {
		t.Fatalf("orphan tar not adopted: %+v", items)
	}
}
