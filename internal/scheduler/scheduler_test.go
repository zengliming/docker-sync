package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-sync/internal/cas"
	"docker-sync/internal/model"
	"docker-sync/internal/registry"
	"docker-sync/internal/store"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fakeRegistry is a RegistryAPI double; Resolve returns a fixed digest and
// Download never touches the network.
type fakeRegistry struct {
	mu               sync.Mutex
	digest           string
	resolveErr       error
	downloads        int
	gate             chan struct{}
	layers           []registry.Layer
	configDigest     string
	downloadProgress bool
	downloadErr      error
}

func (f *fakeRegistry) Resolve(ctx context.Context, ref registry.Ref, platform string) (*registry.Image, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	layers := f.layers
	if layers == nil {
		layers = []registry.Layer{{Digest: "sha256:3333", Size: 10}}
	}
	configDigest := f.configDigest
	if configDigest == "" {
		configDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	}
	return &registry.Image{
		Ref:            ref,
		Platform:       platform,
		ManifestDigest: f.digest,
		ConfigDigest:   configDigest,
		Layers:         layers,
	}, nil
}

func (f *fakeRegistry) Download(_ context.Context, _ *registry.Image, _ registry.BlobSink, progress func(int, int, registry.Layer)) (registry.DownloadStats, error) {
	f.mu.Lock()
	f.downloads++
	layers := f.layers
	if layers == nil {
		layers = []registry.Layer{{Digest: "sha256:3333", Size: 10}}
	}
	report := f.downloadProgress
	err := f.downloadErr
	f.mu.Unlock()
	if report && progress != nil {
		for i, layer := range layers {
			progress(i+1, len(layers), layer)
		}
	}
	return registry.DownloadStats{}, err
}

func (f *fakeRegistry) downloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads
}

type schedEnv struct {
	t     *testing.T
	store *store.Store
	blobs *cas.Store
	sched *Scheduler
	fake  *fakeRegistry
	ctx   context.Context
}

func newSchedEnv(t *testing.T) *schedEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Options{Dir: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	blobs, err := cas.New(filepath.Join(dir, "blobs"))
	if err != nil {
		st.Close()
		t.Fatalf("cas.New: %v", err)
	}
	fake := &fakeRegistry{digest: testDigest}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sched, err := New(Options{
		Store:         st,
		Blobs:         blobs,
		Logger:        logger,
		ClientFactory: func(string) RegistryAPI { return fake },
	})
	if err != nil {
		st.Close()
		t.Fatalf("scheduler.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := sched.Start(ctx); err != nil {
		cancel()
		st.Close()
		t.Fatalf("scheduler.Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := sched.Shutdown(shutdownCtx); err != nil {
			t.Errorf("scheduler.Shutdown: %v", err)
		}
		cancel()
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return &schedEnv{t: t, store: st, blobs: blobs, sched: sched, fake: fake, ctx: ctx}
}

func (e *schedEnv) createTask(mutate func(*model.Task)) model.Task {
	e.t.Helper()
	task := model.Task{
		Name:            "alpine",
		Image:           "alpine:3.20",
		Platform:        "linux/amd64",
		ScheduleType:    model.ScheduleInterval,
		IntervalMinutes: 60,
		AutoSync:        true,
		KeepVersions:    3,
		Enabled:         true,
	}
	if mutate != nil {
		mutate(&task)
	}
	created, err := e.store.CreateTask(task)
	if err != nil {
		e.t.Fatalf("CreateTask: %v", err)
	}
	return created
}

func (e *schedEnv) waitRunDone(runID string) model.Run {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, ok := e.store.GetRun(runID)
		if !ok {
			e.t.Fatalf("run %s vanished", runID)
		}
		if run.Status != model.RunStatusRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	run, _ := e.store.GetRun(runID)
	e.t.Fatalf("run %s did not finish, last status %s phase %s", runID, run.Status, run.Phase)
	return model.Run{}
}

func TestTriggerSyncSkipsWhenDigestUnchanged(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(nil)

	const tarName = "library_alpine_3.20_linux_amd64_111111111111.tar"
	if err := os.WriteFile(env.store.TarPath(tarName), []byte("fake tar"), 0o644); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	env.store.PutTarFile(store.TarFile{Name: tarName, Size: 8, ModTime: time.Now(), TaskID: created.ID})
	env.store.PatchTask(created.ID, func(task *model.Task) {
		task.LastDigest = testDigest
		task.LatestTar = tarName
		task.LatestTarSize = 8
	})

	run, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusSkipped || done.Phase != model.PhaseDone {
		t.Fatalf("run = %s/%s, want skipped/done (logs %+v)", done.Status, done.Phase, done.Logs)
	}
	if done.RemoteDigest != testDigest || done.PreviousDigest != testDigest {
		t.Fatalf("run digests = %+v", done)
	}
	if env.fake.downloadCount() != 0 {
		t.Fatal("skipped run must not download blobs")
	}
	task, _ := env.store.GetTask(created.ID)
	if task.Status != model.TaskStatusIdle || task.LatestTar != tarName {
		t.Fatalf("task = %+v", task)
	}
	if task.LastCheckedAt == nil || task.UpdateAvailable {
		t.Fatalf("task check state = %+v", task)
	}
}

func TestTriggerSyncBusyWhileRunInFlight(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.gate = make(chan struct{})
	created := env.createTask(nil)

	first, err := env.sched.TriggerSync(created.ID, model.TriggerManual, true)
	if err != nil {
		t.Fatalf("first TriggerSync: %v", err)
	}
	if first.Status != model.RunStatusRunning || first.Phase != model.PhaseQueued {
		t.Fatalf("first run snapshot = %s/%s", first.Status, first.Phase)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := env.store.GetTask(created.ID)
		if task.Status == model.TaskStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("second TriggerSync error = %v, want ErrBusy", err)
	}
	if _, err := env.sched.StartCheck(created.ID, model.TriggerSchedule); !errors.Is(err, ErrBusy) {
		t.Fatalf("StartCheck error = %v, want ErrBusy", err)
	}

	close(env.fake.gate)
	done := env.waitRunDone(first.ID)
	if done.Status != model.RunStatusError {
		t.Fatalf("run status = %s, want error (fake download refuses)", done.Status)
	}
	task, _ := env.store.GetTask(created.ID)
	if task.Status != model.TaskStatusError || task.LastError == "" {
		t.Fatalf("task after failure = %+v", task)
	}
}

func TestCheckNowReportsUpdate(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(nil)
	env.store.PatchTask(created.ID, func(task *model.Task) { task.LastDigest = "sha256:old" })

	res, err := env.sched.CheckNow(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("CheckNow: %v", err)
	}
	if !res.UpdateAvailable || res.RemoteDigest != testDigest || res.CurrentDigest != "sha256:old" {
		t.Fatalf("result = %+v", res)
	}
	if res.Image != "docker.io/library/alpine:3.20" || res.Platform != "linux/amd64" {
		t.Fatalf("result ref = %q platform %q", res.Image, res.Platform)
	}
	task, _ := env.store.GetTask(created.ID)
	if !task.UpdateAvailable || task.RemoteDigest != testDigest || task.Status != model.TaskStatusIdle {
		t.Fatalf("task = %+v", task)
	}
	runs := env.store.ListRuns(5, created.ID)
	if len(runs) != 1 || runs[0].Trigger != model.TriggerCheck || runs[0].Status != model.RunStatusSuccess {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestCheckNowInvalidTask(t *testing.T) {
	env := newSchedEnv(t)
	if _, err := env.sched.CheckNow(context.Background(), "t_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}

	broken := env.createTask(func(task *model.Task) { task.Image = "!!!not-an-image" })
	if _, err := env.sched.CheckNow(context.Background(), broken.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

func TestComputeNextIntervalAndCron(t *testing.T) {
	env := newSchedEnv(t)
	now := time.Now()

	checked := now.Add(-3 * time.Minute)
	next := env.sched.computeNext(model.Task{ScheduleType: model.ScheduleInterval, IntervalMinutes: 10, LastCheckedAt: &checked}, now)
	if next == nil {
		t.Fatal("interval next = nil")
	}
	if diff := next.Sub(now); diff < 6*time.Minute || diff > 7*time.Minute+5*time.Second {
		t.Fatalf("interval next in %v, want about 7m", diff)
	}

	stale := now.Add(-30 * time.Minute)
	next = env.sched.computeNext(model.Task{ScheduleType: model.ScheduleInterval, IntervalMinutes: 10, LastCheckedAt: &stale}, now)
	if next == nil || next.Before(now) {
		t.Fatalf("overdue next = %v, want >= now", next)
	}

	next = env.sched.computeNext(model.Task{ScheduleType: model.ScheduleCron, Cron: "*/15 * * * *"}, now)
	if next == nil {
		t.Fatal("cron next = nil")
	}
	if next.Minute()%15 != 0 {
		t.Fatalf("cron next = %v, want quarter hour", next)
	}

	if next := env.sched.computeNext(model.Task{ScheduleType: model.ScheduleCron, Cron: "bogus"}, now); next != nil {
		t.Fatalf("invalid cron next = %v, want nil", next)
	}
}

func TestTaskChangedRecomputesNextRunAt(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(nil)
	env.sched.TaskChanged(created.ID)
	task, _ := env.store.GetTask(created.ID)
	if task.NextRunAt == nil || task.NextRunAt.Before(time.Now()) {
		t.Fatalf("nextRunAt = %v", task.NextRunAt)
	}

	cronTask := env.createTask(func(task *model.Task) {
		task.ScheduleType = model.ScheduleCron
		task.Cron = "0 4 * * *"
	})
	env.sched.TaskChanged(cronTask.ID)
	task, _ = env.store.GetTask(cronTask.ID)
	if task.NextRunAt == nil || task.NextRunAt.Hour() != 4 || task.NextRunAt.Minute() != 0 {
		t.Fatalf("cron nextRunAt = %v", task.NextRunAt)
	}

	env.sched.TaskDeleted(created.ID)
	env.store.DeleteTask(created.ID)
	if _, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TriggerSync after delete = %v, want ErrNotFound", err)
	}
	if task, _ := env.store.GetTask(created.ID); task.ID != "" {
		t.Fatalf("task still present: %+v", task)
	}
}

func TestScheduledCheckSkipsDisabledTask(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(func(task *model.Task) { task.Enabled = false })
	if _, err := env.sched.StartCheck(created.ID, model.TriggerSchedule); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %v, want disabled", err)
	}
}

// TestProgressCountsOnlyNetworkBytes guards the live bytesDownloaded counter:
// blobs reused from the CAS must not be reported as network traffic.
func TestProgressCountsOnlyNetworkBytes(t *testing.T) {
	content := []byte("layer-payload-bytes")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	cases := []struct {
		name      string
		preload   bool
		wantBytes int64
	}{
		{name: "cached blob is not network traffic", preload: true, wantBytes: 0},
		{name: "missing blob is network traffic", preload: false, wantBytes: int64(len(content))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSchedEnv(t)
			env.fake.mu.Lock()
			env.fake.layers = []registry.Layer{{Digest: digest, Size: int64(len(content))}}
			env.fake.downloadProgress = true
			env.fake.downloadErr = errors.New("模拟下载中断")
			env.fake.mu.Unlock()

			if tc.preload {
				if _, err := env.blobs.PutBytes(context.Background(), digest, content); err != nil {
					t.Fatalf("PutBytes: %v", err)
				}
			}
			created := env.createTask(func(task *model.Task) {
				task.LastDigest = "sha256:" + strings.Repeat("a", 64)
			})
			run, err := env.sched.TriggerSync(created.ID, model.TriggerManual, true)
			if err != nil {
				t.Fatalf("TriggerSync: %v", err)
			}
			done := env.waitRunDone(run.ID)
			if done.Status != model.RunStatusError {
				t.Fatalf("run status = %s, want error (fake download fails)", done.Status)
			}
			if done.BytesDownloaded != tc.wantBytes {
				t.Fatalf("bytesDownloaded = %d, want %d", done.BytesDownloaded, tc.wantBytes)
			}
		})
	}
}
