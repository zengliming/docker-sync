// Package scheduler drives image synchronisation: it schedules periodic
// digest checks (cron or interval), runs manual sync/check requests, limits
// the number of concurrent runs and executes the resolve/download/pack
// pipeline against the registry and the tar builder.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"docker-sync/internal/cas"
	"docker-sync/internal/model"
	"docker-sync/internal/registry"
	"docker-sync/internal/store"
)

const (
	// intervalTick is how often the interval scheduler wakes up to compare each
	// task's NextRunAt with the current time.
	intervalTick = 30 * time.Second
	// blobGCTick is how often the blob cache is pruned (besides at startup).
	blobGCTick = 6 * time.Hour
	// queueSize bounds the number of runs waiting for a worker slot.
	queueSize = 256
	// shutdownWait is the maximum time Shutdown gives running jobs to finish.
	shutdownWait = 30 * time.Second
	// minRunTimeout guards registry calls against hanging forever.
	runResolveTimeout = 3 * time.Minute
)

// Sentinel errors translated into HTTP status codes by the API layer.
var (
	// ErrNotFound means the task does not exist.
	ErrNotFound = errors.New("task not found")
	// ErrBusy means the task already has a run in flight.
	ErrBusy = errors.New("task already has a run in progress")
	// ErrNotStarted means the scheduler has not been started (or was stopped).
	ErrNotStarted = errors.New("scheduler is not running")
	// ErrInvalidInput means the task configuration cannot be executed.
	ErrInvalidInput = errors.New("invalid task")
	// errDisabled means a scheduled trigger found the task disabled.
	errDisabled = errors.New("task is disabled")
	// errIgnoredLatest means a scheduled trigger was skipped because the task
	// watches the moving "latest" tag and asked for it to be ignored.
	errIgnoredLatest = errors.New("task ignores the latest tag")
)

// cronParser accepts exactly the standard five fields (minute hour dom month
// dow), matching the contract.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ValidateCron reports whether spec is a valid five field cron expression.
func ValidateCron(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return errors.New("cron 表达式不能为空")
	}
	if _, err := cronParser.Parse(spec); err != nil {
		return fmt.Errorf("cron 表达式 %q 无效: %w", spec, err)
	}
	return nil
}

// RegistryAPI is the subset of registry.Client used by the scheduler. It is an
// interface so tests can inject a fake without network access.
type RegistryAPI interface {
	Resolve(ctx context.Context, ref registry.Ref, platform string) (*registry.Image, error)
	Download(ctx context.Context, img *registry.Image, sink registry.BlobSink, progress func(done, total int, layer registry.Layer)) (registry.DownloadStats, error)
}

// Options configures New.
type Options struct {
	// Store is the persistent state (required).
	Store *store.Store
	// Blobs is the content addressed layer cache (required).
	Blobs *cas.Store
	// Logger receives scheduler diagnostics; defaults to slog.Default().
	Logger *slog.Logger
	// ClientFactory, when set, creates registry clients (tests only).
	ClientFactory func(host string) RegistryAPI
}

// jobKind distinguishes the two kinds of runs.
type jobKind int

const (
	kindSync jobKind = iota + 1
	kindCheck
)

// job is one queued run waiting for a worker slot.
type job struct {
	kind    jobKind
	runID   string
	taskID  string
	trigger string
	force   bool
}

// Scheduler owns the cron entries, the run queue and the worker bookkeeping.
type Scheduler struct {
	store *store.Store
	blobs *cas.Store
	log   *slog.Logger

	clientFactory func(host string) RegistryAPI

	jobs       chan *job
	workerDone chan struct{}

	cron        *cron.Cron
	cronMu      sync.Mutex
	cronEntries map[string]cron.EntryID

	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	started    bool
	active     map[string]string
	runCancels map[string]context.CancelFunc
	running    int
	clients    map[string]RegistryAPI

	wg sync.WaitGroup
}

// New creates a scheduler. Call Start to begin processing.
func New(opts Options) (*Scheduler, error) {
	if opts.Store == nil {
		return nil, errors.New("scheduler: store is required")
	}
	if opts.Blobs == nil {
		return nil, errors.New("scheduler: blob store is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		store:         opts.Store,
		blobs:         opts.Blobs,
		log:           logger,
		clientFactory: opts.ClientFactory,
		jobs:          make(chan *job, queueSize),
		workerDone:    make(chan struct{}, 1),
		cronEntries:   make(map[string]cron.EntryID),
		active:        make(map[string]string),
		runCancels:    make(map[string]context.CancelFunc),
		clients:       make(map[string]RegistryAPI),
	}, nil
}

// Start repairs state left behind by a crash, installs cron entries and starts
// the dispatcher, interval ticker and blob GC loops.
func (s *Scheduler) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("scheduler: already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.ctx = runCtx
	s.cancel = cancel
	s.started = true
	s.mu.Unlock()

	if tasks, runs := s.store.RecoverAfterRestart(time.Now()); tasks > 0 || runs > 0 {
		s.log.Info("已修复上次异常退出遗留的运行状态", "tasks", tasks, "runs", runs)
	}

	s.cronMu.Lock()
	s.cron = cron.New(cron.WithParser(cronParser), cron.WithLogger(cronLogger{log: s.log}))
	s.cron.Start()
	s.cronMu.Unlock()
	s.rebuildCronEntries()
	s.initNextRunTimes()

	s.wg.Add(3)
	go s.dispatchLoop()
	go s.intervalLoop()
	go s.gcLoop()
	s.log.Info("调度器已启动")
	return nil
}

// Shutdown cancels the loops, waits at most 30 seconds for running jobs and
// marks every still running run as canceled.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	if c := s.cron; c != nil {
		select {
		case <-c.Stop().Done():
		case <-ctx.Done():
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	wait, waitCancel := context.WithTimeout(ctx, shutdownWait)
	defer waitCancel()

	var err error
	select {
	case <-done:
	case <-wait.Done():
		err = fmt.Errorf("scheduler: 等待运行结束超时: %w", wait.Err())
		s.log.Warn("等待运行结束超时，强制取消未完成的运行")
	}

	now := time.Now()
	for _, r := range s.store.RunningRuns() {
		s.finishRun(r.ID, model.RunStatusCanceled, "服务关闭，运行已取消", nil, now)
		s.store.PatchTask(r.TaskID, func(t *model.Task) {
			if t.Status != model.TaskStatusPaused {
				t.Status = model.TaskStatusIdle
			}
		})
	}
	s.log.Info("调度器已停止")
	return err
}

// Started reports whether the scheduler is currently running.
func (s *Scheduler) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// RunningCount reports how many runs currently occupy a worker slot.
func (s *Scheduler) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// dispatchLoop hands queued jobs to workers while a slot is free.
func (s *Scheduler) dispatchLoop() {
	defer s.wg.Done()
	pending := make([]*job, 0, 8)
	for {
		s.pump(&pending)
		select {
		case <-s.ctx.Done():
			// Runs that never started are marked canceled so their history is
			// not left dangling in running state.
			for _, j := range pending {
				s.finishRun(j.runID, model.RunStatusCanceled, "服务正在关闭，运行已取消", nil, time.Now())
				s.clearActive(j.taskID, j.runID)
				s.store.PatchTask(j.taskID, func(t *model.Task) { t.Status = model.TaskStatusIdle })
			}
			return
		case j := <-s.jobs:
			pending = append(pending, j)
		case <-s.workerDone:
		}
	}
}

// pump starts as many pending jobs as the concurrency limit allows.
func (s *Scheduler) pump(pending *[]*job) {
	limit := s.store.SnapshotSettings().MaxConcurrentRuns
	if limit < 1 {
		limit = 1
	}
	for len(*pending) > 0 {
		s.mu.Lock()
		if !s.started || s.running >= limit {
			s.mu.Unlock()
			return
		}
		j := (*pending)[0]
		*pending = (*pending)[1:]
		s.running++
		s.wg.Add(1)
		s.mu.Unlock()
		go s.runJob(j)
	}
}

// runJob executes one job and releases its bookkeeping afterwards.
func (s *Scheduler) runJob(j *job) {
	defer s.wg.Done()

	followUp := false
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("运行发生 panic", "run", j.runID, "task", j.taskID, "panic", rec)
				msg := fmt.Sprintf("内部错误: %v", rec)
				s.finishRun(j.runID, model.RunStatusError, msg, nil, time.Now())
				s.store.PatchTask(j.taskID, func(t *model.Task) {
					t.Status = model.TaskStatusError
					t.LastError = msg
				})
			}
		}()
		switch j.kind {
		case kindSync:
			s.executeSync(j)
		case kindCheck:
			followUp = s.executeCheck(j)
		}
	}()

	s.mu.Lock()
	if s.running > 0 {
		s.running--
	}
	s.mu.Unlock()

	s.clearActive(j.taskID, j.runID)
	s.reschedule(j.taskID)

	// Wake the dispatcher so the freed slot can be reused.
	select {
	case s.workerDone <- struct{}{}:
	case <-s.ctx.Done():
	}

	if followUp && s.Started() {
		if _, err := s.TriggerSync(j.taskID, model.TriggerSchedule, false); err != nil && !errors.Is(err, ErrBusy) {
			s.log.Warn("自动同步启动失败", "task", j.taskID, "error", err)
		}
	}
}

// intervalLoop compares each interval task's NextRunAt with the clock.
func (s *Scheduler) intervalLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(intervalTick)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkDueTasks()
		}
	}
}

// checkDueTasks starts a scheduled check for every interval task that is due.
func (s *Scheduler) checkDueTasks() {
	now := time.Now()
	for _, t := range s.store.ListTasks() {
		if !t.Enabled || t.ScheduleType != model.ScheduleInterval {
			continue
		}
		if t.NextRunAt == nil {
			// 没有下次运行时间：任务被忽略 latest 标记排除了定时调度。
			continue
		}
		if now.Before(*t.NextRunAt) {
			continue
		}
		entry := t
		if _, err := s.StartCheck(entry.ID, model.TriggerSchedule); err != nil {
			if errors.Is(err, ErrBusy) || errors.Is(err, errIgnoredLatest) {
				continue
			}
			s.log.Warn("定时检查启动失败", "task", entry.ID, "image", entry.Image, "error", err)
			continue
		}
	}
}

// gcLoop prunes the blob cache once at startup and then periodically.
func (s *Scheduler) gcLoop() {
	defer s.wg.Done()
	s.runBlobGC()
	ticker := time.NewTicker(blobGCTick)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.runBlobGC()
		}
	}
}

// TaskChanged recomputes the schedule after an edit or creation.
func (s *Scheduler) TaskChanged(taskID string) {
	task, ok := s.store.GetTask(taskID)
	if !ok {
		return
	}
	s.syncCronEntry(&task)
	var next *time.Time
	if task.Enabled {
		if n := s.computeNext(task, time.Now()); n != nil {
			next = n
		}
	}
	s.store.PatchTask(taskID, func(t *model.Task) {
		if s.started {
			t.NextRunAt = next
		}
	})
}

// TaskDeleted removes the cron entry and cancels a run in flight.
func (s *Scheduler) TaskDeleted(taskID string) {
	s.cronMu.Lock()
	if id, ok := s.cronEntries[taskID]; ok {
		if s.cron != nil {
			s.cron.Remove(id)
		}
		delete(s.cronEntries, taskID)
	}
	s.cronMu.Unlock()

	s.mu.Lock()
	runID := s.active[taskID]
	cancel := s.runCancels[runID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// SettingsChanged drops cached registry clients and wakes the dispatcher so a
// new concurrency limit takes effect immediately.
func (s *Scheduler) SettingsChanged() {
	s.mu.Lock()
	s.clients = make(map[string]RegistryAPI)
	s.mu.Unlock()
	select {
	case s.workerDone <- struct{}{}:
	case <-s.ctx.Done():
	}
}

// rebuildCronEntries (re)installs a cron entry for every enabled cron task.
func (s *Scheduler) rebuildCronEntries() {
	for _, task := range s.store.ListTasks() {
		t := task
		s.syncCronEntry(&t)
	}
}

// syncCronEntry installs, replaces or removes the cron entry of one task.
func (s *Scheduler) syncCronEntry(task *model.Task) {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	if s.cron == nil {
		return
	}
	if old, ok := s.cronEntries[task.ID]; ok {
		s.cron.Remove(old)
		delete(s.cronEntries, task.ID)
	}
	if !task.Enabled || task.ScheduleType != model.ScheduleCron {
		return
	}
	if latestTagIgnored(*task, model.TriggerSchedule) {
		// 忽略 latest 的任务只保留手动触发，不安装 cron 条目。
		return
	}
	spec := strings.TrimSpace(task.Cron)
	if _, err := cronParser.Parse(spec); err != nil {
		s.log.Warn("任务 cron 表达式无效，忽略该任务", "task", task.ID, "cron", task.Cron, "error", err)
		return
	}
	taskID := task.ID
	image := task.Image
	id, err := s.cron.AddFunc(spec, func() {
		if _, err := s.StartCheck(taskID, model.TriggerSchedule); err != nil {
			if errors.Is(err, ErrBusy) || errors.Is(err, errDisabled) || errors.Is(err, errIgnoredLatest) {
				return
			}
			s.log.Warn("cron 检查启动失败", "task", taskID, "image", image, "error", err)
		}
	})
	if err != nil {
		s.log.Warn("添加 cron 定时任务失败", "task", task.ID, "cron", task.Cron, "error", err)
		return
	}
	s.cronEntries[task.ID] = id
}

// initNextRunTimes publishes the first NextRunAt value for every enabled task.
func (s *Scheduler) initNextRunTimes() {
	now := time.Now()
	for _, task := range s.store.ListTasks() {
		t := task
		if !t.Enabled {
			continue
		}
		next := s.computeNext(t, now)
		if next == nil {
			continue
		}
		n := *next
		s.store.PatchTask(t.ID, func(task *model.Task) { task.NextRunAt = &n })
	}
}

// reschedule recomputes NextRunAt after a run finished.
func (s *Scheduler) reschedule(taskID string) {
	if !s.Started() {
		return
	}
	task, ok := s.store.GetTask(taskID)
	if !ok {
		return
	}
	var next *time.Time
	if task.Enabled {
		next = s.computeNext(task, time.Now())
	}
	s.store.PatchTask(taskID, func(t *model.Task) { t.NextRunAt = next })
}

// computeNext returns the next scheduled check time for a task.
func (s *Scheduler) computeNext(task model.Task, now time.Time) *time.Time {
	if latestTagIgnored(task, model.TriggerSchedule) {
		// 忽略 latest 的任务不做定时检查，只保留手动同步。
		return nil
	}
	if task.ScheduleType == model.ScheduleCron {
		spec := strings.TrimSpace(task.Cron)
		sched, err := cronParser.Parse(spec)
		if err != nil {
			return nil
		}
		next := sched.Next(now)
		if next.IsZero() {
			return nil
		}
		return &next
	}
	interval := time.Duration(task.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = time.Hour
	}
	base := task.CreatedAt
	if task.LastCheckedAt != nil {
		base = *task.LastCheckedAt
	} else if task.LastSyncedAt != nil && task.LastSyncedAt.After(base) {
		base = *task.LastSyncedAt
	}
	next := base.Add(interval)
	if next.Before(now) {
		next = now
	}
	return &next
}

// reserve marks the task as busy for a soon to be created run.
func (s *Scheduler) reserve(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return false
	}
	if _, busy := s.active[taskID]; busy {
		return false
	}
	s.active[taskID] = ""
	return true
}

// setActive replaces the reservation placeholder with the real run ID.
func (s *Scheduler) setActive(taskID, runID string) {
	s.mu.Lock()
	s.active[taskID] = runID
	s.mu.Unlock()
}

// clearActive releases the task slot when the run is done.
func (s *Scheduler) clearActive(taskID, runID string) {
	s.mu.Lock()
	if cur, ok := s.active[taskID]; ok && (runID == "" || cur == runID) {
		delete(s.active, taskID)
	}
	s.mu.Unlock()
}

// isActive reports whether a run for the task is queued or executing.
func (s *Scheduler) isActive(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, busy := s.active[taskID]
	return busy
}

// newRunContext derives the context a worker listens to for cancellation.
func (s *Scheduler) newRunContext(runID string) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	parent := s.ctx
	s.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.runCancels[runID] = cancel
	s.mu.Unlock()
	return ctx, cancel
}

// releaseRunContext forgets a finished run and releases its context.
func (s *Scheduler) releaseRunContext(runID string, cancel context.CancelFunc) {
	cancel()
	s.mu.Lock()
	delete(s.runCancels, runID)
	s.mu.Unlock()
}

// enqueue hands a job to the dispatcher, or cancels it when shutting down.
func (s *Scheduler) enqueue(j *job) {
	s.mu.Lock()
	ctx := s.ctx
	started := s.started
	s.mu.Unlock()
	if !started || ctx == nil {
		s.finishRun(j.runID, model.RunStatusCanceled, "调度器未运行，运行已取消", nil, time.Now())
		s.clearActive(j.taskID, j.runID)
		return
	}
	select {
	case s.jobs <- j:
	case <-ctx.Done():
		s.finishRun(j.runID, model.RunStatusCanceled, "服务正在关闭，运行已取消", nil, time.Now())
		s.clearActive(j.taskID, j.runID)
		s.store.PatchTask(j.taskID, func(t *model.Task) { t.Status = model.TaskStatusIdle })
	}
}

// startRun marks a queued run as running in the resolving phase.
func (s *Scheduler) startRun(j *job, task model.Task, phase string) {
	now := time.Now()
	s.store.PatchRun(j.runID, func(r *model.Run) {
		r.Status = model.RunStatusRunning
		r.Phase = phase
		if r.StartedAt.IsZero() {
			r.StartedAt = now
		}
	})
	s.store.PatchTask(j.taskID, func(t *model.Task) {
		t.Status = model.TaskStatusRunning
		t.LastRunID = j.runID
	})
	s.logRun(j.runID, model.LevelInfo, "开始%s：%s（平台 %s）", triggerLabel(j.trigger), task.Image, task.Platform)
}

// updateRun patches one run.
func (s *Scheduler) updateRun(runID string, fn func(*model.Run)) {
	s.store.PatchRun(runID, fn)
}

// finishRun completes a run with the given status.
func (s *Scheduler) finishRun(runID, status, errMsg string, mutate func(*model.Run), now time.Time) {
	s.store.PatchRun(runID, func(r *model.Run) {
		if mutate != nil {
			mutate(r)
		}
		r.Status = status
		r.Phase = model.PhaseDone
		if r.FinishedAt == nil {
			r.FinishedAt = &now
		}
		if errMsg != "" {
			r.Error = errMsg
		}
	})
}

// logRun appends one log line to a run.
func (s *Scheduler) logRun(runID, level, format string, args ...any) {
	s.store.AppendRunLog(runID, level, fmt.Sprintf(format, args...))
}

// triggerLabel renders a trigger for log messages.
func triggerLabel(trigger string) string {
	switch trigger {
	case model.TriggerSchedule:
		return "定时同步"
	case model.TriggerCheck:
		return "手动检查"
	case model.TriggerUpload:
		return "手动上传 SMB"
	default:
		return "手动同步"
	}
}

// cronLogger adapts slog to the robfig/cron logger interface so cron noise is
// routed through the service logger.
type cronLogger struct {
	log *slog.Logger
}

func (l cronLogger) Info(msg string, keysAndValues ...any) {
	l.log.Debug("cron: "+msg, keysAndValues...)
}

func (l cronLogger) Error(err error, msg string, keysAndValues ...any) {
	args := append([]any{"error", err}, keysAndValues...)
	l.log.Warn("cron: "+msg, args...)
}
