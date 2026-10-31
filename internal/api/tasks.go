package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"docker-sync/internal/model"
	"docker-sync/internal/registry"
	"docker-sync/internal/scheduler"
)

// applyTaskInput validates the create/update payload and returns the task to
// persist. Run related fields are inherited from base (zero for create).
func (s *Server) applyTaskInput(base model.Task, in model.TaskInput) (model.Task, error) {
	t := base

	image := strings.TrimSpace(in.Image)
	if image == "" {
		return t, errors.New("image 必填")
	}
	ref, err := registry.Parse(image)
	if err != nil {
		return t, fmt.Errorf("image %q 无法解析: %v", image, err)
	}
	t.Image = ref.String()

	platform := strings.TrimSpace(in.Platform)
	if platform == "" {
		platform = s.store.GetSettings().DefaultPlatform
	}
	if strings.TrimSpace(platform) == "" {
		platform = "linux/amd64"
	}
	p, err := registry.ParsePlatform(platform)
	if err != nil {
		return t, fmt.Errorf("platform %q 非法: %v", in.Platform, err)
	}
	t.Platform = p.String()

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = t.Image
	}
	t.Name = name

	switch strings.TrimSpace(in.ScheduleType) {
	case "", model.ScheduleInterval:
		t.ScheduleType = model.ScheduleInterval
	case model.ScheduleCron:
		t.ScheduleType = model.ScheduleCron
	default:
		return t, fmt.Errorf("scheduleType %q 非法，只能是 %q 或 %q", in.ScheduleType, model.ScheduleInterval, model.ScheduleCron)
	}

	t.IntervalMinutes = in.IntervalMinutes
	if t.IntervalMinutes <= 0 {
		t.IntervalMinutes = 60
	}
	t.Cron = strings.TrimSpace(in.Cron)
	if t.ScheduleType == model.ScheduleCron {
		if err := scheduler.ValidateCron(t.Cron); err != nil {
			return t, err
		}
	}

	t.AutoSync = in.AutoSync
	t.IgnoreLatest = in.IgnoreLatest
	t.KeepVersions = in.KeepVersions
	if t.KeepVersions <= 0 {
		t.KeepVersions = 3
	}
	if t.KeepVersions > 50 {
		t.KeepVersions = 50
	}
	t.Enabled = in.Enabled
	if t.Enabled {
		if t.Status == "" || t.Status == model.TaskStatusPaused {
			t.Status = model.TaskStatusIdle
		}
	} else {
		t.Status = model.TaskStatusPaused
	}
	return t, nil
}

// handleListTasks returns every task, newest first.
func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	tasks := s.store.ListTasks()
	if tasks == nil {
		tasks = []model.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// handleCreateTask validates TaskInput and creates a task.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var in model.TaskInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "解析请求体失败: %v", err)
		return
	}
	task, err := s.applyTaskInput(model.Task{}, in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "参数校验失败: %v", err)
		return
	}
	created, err := s.store.CreateTask(task)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建任务失败: %v", err)
		return
	}
	s.sched.TaskChanged(created.ID)
	writeJSON(w, http.StatusCreated, created)
}

// handleGetTask returns one task.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.store.GetTask(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "任务不存在: %s", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleUpdateTask replaces the editable fields of a task.
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.store.GetTask(id)
	if !ok {
		writeError(w, http.StatusNotFound, "任务不存在: %s", id)
		return
	}
	var in model.TaskInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "解析请求体失败: %v", err)
		return
	}
	updated, err := s.applyTaskInput(existing, in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "参数校验失败: %v", err)
		return
	}
	saved, err := s.store.UpdateTask(updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "更新任务失败: %v", err)
		return
	}
	s.sched.TaskChanged(saved.ID)
	writeJSON(w, http.StatusOK, saved)
}

// handleDeleteTask removes a task and its tar artifacts.
func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.GetTask(id); !ok {
		writeError(w, http.StatusNotFound, "任务不存在: %s", id)
		return
	}
	s.sched.TaskDeleted(id)
	if !s.store.DeleteTask(id) {
		writeError(w, http.StatusNotFound, "任务不存在: %s", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSyncTask queues a manual sync run and returns it immediately.
func (s *Server) handleSyncTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Force bool `json:"force"`
	}
	if err := decodeJSON(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "解析请求体失败: %v", err)
		return
	}
	run, err := s.sched.TriggerSync(id, model.TriggerManual, body.Force)
	if err != nil {
		s.writeSchedulerError(w, err, "启动同步失败")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

// handleCheckTask resolves the remote digest synchronously.
func (s *Server) handleCheckTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.sched.CheckNow(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, scheduler.ErrNotFound):
			writeError(w, http.StatusNotFound, "任务不存在: %s", id)
		case errors.Is(err, scheduler.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, "检查更新失败: %v", err)
		default:
			writeError(w, http.StatusBadGateway, "检查更新失败: %v", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updateAvailable": res.UpdateAvailable,
		"remoteDigest":    res.RemoteDigest,
		"currentDigest":   res.CurrentDigest,
		"image":           res.Image,
		"platform":        res.Platform,
	})
}

// handleTaskRuns lists runs of one task (without logs).
func (s *Server) handleTaskRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.GetTask(id); !ok {
		writeError(w, http.StatusNotFound, "任务不存在: %s", id)
		return
	}
	runs := s.store.ListRuns(parseLimit(r, 50, 200), id)
	if runs == nil {
		runs = []model.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// writeSchedulerError maps scheduler sentinel errors to contract statuses.
func (s *Server) writeSchedulerError(w http.ResponseWriter, err error, context string) {
	switch {
	case errors.Is(err, scheduler.ErrNotFound):
		writeError(w, http.StatusNotFound, "任务不存在")
	case errors.Is(err, scheduler.ErrBusy):
		writeError(w, http.StatusConflict, "任务已有正在进行的运行，请稍后再试")
	case errors.Is(err, scheduler.ErrNotStarted):
		writeError(w, http.StatusInternalServerError, "调度器尚未就绪，请稍后再试")
	default:
		writeError(w, http.StatusInternalServerError, "%s: %v", context, err)
	}
}
