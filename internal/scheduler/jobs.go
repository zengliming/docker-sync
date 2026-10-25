package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-sync/internal/model"
	"docker-sync/internal/registry"
	"docker-sync/internal/save"
	"docker-sync/internal/store"
)

// CheckResult is the outcome of a manual update check.
type CheckResult struct {
	UpdateAvailable bool
	RemoteDigest    string
	CurrentDigest   string
	Image           string
	Platform        string
}

// TriggerSync queues a full synchronisation of the task. trigger is
// model.TriggerManual for user triggered syncs and model.TriggerSchedule for
// automatic follow-up syncs. force bypasses the digest skip optimisation.
func (s *Scheduler) TriggerSync(taskID, trigger string, force bool) (model.Run, error) {
	if trigger == "" {
		trigger = model.TriggerManual
	}
	task, ok := s.store.GetTask(taskID)
	if !ok {
		return model.Run{}, ErrNotFound
	}
	if !s.Started() {
		return model.Run{}, ErrNotStarted
	}
	if !s.reserve(taskID) {
		return model.Run{}, ErrBusy
	}
	run, err := s.store.CreateRun(model.Run{
		TaskID:   task.ID,
		TaskName: task.Name,
		Image:    task.Image,
		Platform: s.displayPlatform(task),
		Trigger:  trigger,
	})
	if err != nil {
		s.clearActive(taskID, "")
		return model.Run{}, fmt.Errorf("创建运行记录失败: %w", err)
	}
	s.setActive(taskID, run.ID)
	s.store.PatchTask(taskID, func(t *model.Task) {
		t.LastRunID = run.ID
		t.Status = model.TaskStatusQueued
	})
	s.enqueue(&job{kind: kindSync, runID: run.ID, taskID: task.ID, trigger: trigger, force: force})
	return run, nil
}

// StartCheck queues a scheduled or manual check-only run.
func (s *Scheduler) StartCheck(taskID, trigger string) (model.Run, error) {
	task, ok := s.store.GetTask(taskID)
	if !ok {
		return model.Run{}, ErrNotFound
	}
	if trigger == model.TriggerSchedule && !task.Enabled {
		return model.Run{}, fmt.Errorf("%w: %s", errDisabled, taskID)
	}
	if !s.Started() {
		return model.Run{}, ErrNotStarted
	}
	if !s.reserve(taskID) {
		return model.Run{}, ErrBusy
	}
	run, err := s.store.CreateRun(model.Run{
		TaskID:   task.ID,
		TaskName: task.Name,
		Image:    task.Image,
		Platform: s.displayPlatform(task),
		Trigger:  trigger,
	})
	if err != nil {
		s.clearActive(taskID, "")
		return model.Run{}, fmt.Errorf("创建运行记录失败: %w", err)
	}
	s.setActive(taskID, run.ID)
	s.store.PatchTask(taskID, func(t *model.Task) {
		t.LastRunID = run.ID
		if t.Status != model.TaskStatusPaused {
			t.Status = model.TaskStatusQueued
		}
	})
	s.enqueue(&job{kind: kindCheck, runID: run.ID, taskID: task.ID, trigger: trigger})
	return run, nil
}

// CheckNow resolves the remote digest synchronously and reports whether an
// update is available. It is used by POST /api/tasks/{id}/check.
func (s *Scheduler) CheckNow(ctx context.Context, taskID string) (CheckResult, error) {
	task, ok := s.store.GetTask(taskID)
	if !ok {
		return CheckResult{}, ErrNotFound
	}
	ref, err := registry.Parse(task.Image)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%w: 解析镜像引用 %q 失败: %v", ErrInvalidInput, task.Image, err)
	}
	platform, err := s.platformFor(task)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%w: 解析平台失败: %v", ErrInvalidInput, err)
	}
	run, err := s.store.CreateRun(model.Run{
		TaskID:   task.ID,
		TaskName: task.Name,
		Image:    ref.String(),
		Platform: platform,
		Trigger:  model.TriggerCheck,
	})
	if err != nil {
		return CheckResult{}, fmt.Errorf("创建运行记录失败: %w", err)
	}
	s.store.PatchTask(taskID, func(t *model.Task) { t.LastRunID = run.ID })

	res, err := s.performCheck(ctx, run.ID, task, ref, platform)
	if err != nil {
		s.failRun(run.ID, taskID, "检查更新失败", err)
		return CheckResult{}, err
	}
	return res, nil
}

// executeSync runs the resolve/download/pack pipeline for one queued job.
func (s *Scheduler) executeSync(j *job) {
	ctx, cancel := s.newRunContext(j.runID)
	defer s.releaseRunContext(j.runID, cancel)

	task, ok := s.store.GetTask(j.taskID)
	if !ok {
		s.finishRun(j.runID, model.RunStatusCanceled, "任务已删除", nil, time.Now())
		return
	}
	s.startRun(j, task, model.PhaseResolving)

	ref, err := registry.Parse(task.Image)
	if err != nil {
		if s.canceled(ctx, j) {
			return
		}
		s.failRun(j.runID, j.taskID, "解析镜像引用失败", err)
		return
	}
	platform, err := s.platformFor(task)
	if err != nil {
		if s.canceled(ctx, j) {
			return
		}
		s.failRun(j.runID, j.taskID, "解析平台失败", err)
		return
	}
	s.updateRun(j.runID, func(r *model.Run) {
		r.Image = ref.String()
		r.Platform = platform
	})
	s.logRun(j.runID, model.LevelInfo, "解析镜像 %s（平台 %s）", ref.String(), platform)

	client := s.clientFor(ref.Registry)
	resolveCtx, resolveCancel := context.WithTimeout(ctx, runResolveTimeout)
	img, err := client.Resolve(resolveCtx, ref, platform)
	resolveCancel()
	if err != nil {
		if s.canceled(ctx, j) {
			return
		}
		s.failRun(j.runID, j.taskID, fmt.Sprintf("解析远端镜像 %s 失败", ref.String()), err)
		return
	}
	s.logRun(j.runID, model.LevelInfo, "远端 manifest digest %s", img.ManifestDigest)

	// Optimisation: identical digest and the previous tar still on disk.
	skip := !j.force && task.LastDigest != "" && img.ManifestDigest == task.LastDigest
	if skip && task.LatestTar != "" && s.store.HasTar(task.LatestTar) {
		now := time.Now()
		s.logRun(j.runID, model.LevelInfo, "镜像无变化（digest %s），跳过下载", shortDigest(img.ManifestDigest))
		s.finishRun(j.runID, model.RunStatusSkipped, "", func(r *model.Run) {
			r.RemoteDigest = img.ManifestDigest
			r.PreviousDigest = task.LastDigest
			r.ConfigDigest = img.ConfigDigest
			r.UpdateAvailable = false
			r.Layers = len(img.Layers)
		}, now)
		s.store.PatchTask(j.taskID, func(t *model.Task) {
			t.LastCheckedAt = &now
			t.RemoteDigest = img.ManifestDigest
			t.UpdateAvailable = false
			t.LastRunID = j.runID
			if t.Status != model.TaskStatusPaused {
				t.Status = model.TaskStatusIdle
			}
			t.LastError = ""
		})
		return
	}
	if skip {
		s.logRun(j.runID, model.LevelWarn, "digest 未变化但本地 tar 已缺失，重新生成")
	}

	// Download all blobs not yet present in the content addressed cache.
	s.updateRun(j.runID, func(r *model.Run) {
		r.Phase = model.PhaseDownloading
		r.Layers = len(img.Layers)
	})
	s.store.PatchTask(j.taskID, func(t *model.Task) { t.Status = model.TaskStatusRunning })
	s.logRun(j.runID, model.LevelInfo, "开始下载 %d 个 blob，并发 %d", len(img.Layers)+1, s.store.SnapshotSettings().MaxConcurrentDownloads)

	// Blobs already in the CAS are reused without touching the network, so the
	// live progress must only count bytes that are actually transferred.
	reusedBefore := make(map[string]struct{})
	if s.blobs.Has(img.ConfigDigest) {
		reusedBefore[img.ConfigDigest] = struct{}{}
	}
	for _, layer := range img.Layers {
		if s.blobs.Has(layer.Digest) {
			reusedBefore[layer.Digest] = struct{}{}
		}
	}
	var processed int64
	stats, err := client.Download(ctx, img, s.blobs, func(done, total int, layer registry.Layer) {
		if _, reused := reusedBefore[layer.Digest]; !reused {
			processed += layer.Size
			s.updateRun(j.runID, func(r *model.Run) {
				r.BytesDownloaded = processed
			})
		}
	})
	if err != nil {
		if s.canceled(ctx, j) {
			return
		}
		s.failRun(j.runID, j.taskID, fmt.Sprintf("下载镜像 %s 失败", ref.String()), err)
		return
	}
	s.logRun(j.runID, model.LevelInfo, "下载完成：传输 %d 字节，新增 %d 个 blob，复用 %d 个 blob",
		stats.BytesDownloaded, stats.BlobsDownloaded, stats.BlobsReused)

	// Pack the image into a docker save compatible tar.
	s.updateRun(j.runID, func(r *model.Run) {
		r.Phase = model.PhasePacking
		r.BytesDownloaded = stats.BytesDownloaded
		r.BlobsReused = stats.BlobsReused
	})
	name := TarName(ref, platform, img.ManifestDigest)
	path := filepath.Join(s.store.TarsDir(), name)
	s.logRun(j.runID, model.LevelInfo, "开始打包 %s", name)
	size, err := save.Build(ctx, path, img, s.blobs, save.Options{
		RepoTags: []string{task.Image},
		Platform: img.Platform,
		Created:  time.Now(),
	})
	if err != nil {
		removeQuietly(path)
		if s.canceled(ctx, j) {
			return
		}
		s.failRun(j.runID, j.taskID, fmt.Sprintf("打包镜像 %s 失败", ref.String()), err)
		return
	}
	if err := save.Validate(path, img); err != nil {
		removeQuietly(path)
		if s.canceled(ctx, j) {
			return
		}
		s.failRun(j.runID, j.taskID, fmt.Sprintf("校验 tar %s 失败", name), err)
		return
	}

	// The task may have been deleted while the archive was being built.
	if _, ok := s.store.GetTask(j.taskID); !ok {
		removeQuietly(path)
		s.finishRun(j.runID, model.RunStatusCanceled, "任务已删除，产物已丢弃", nil, time.Now())
		return
	}

	modTime := time.Now()
	if info, statErr := os.Stat(path); statErr == nil {
		size = info.Size()
		modTime = info.ModTime()
	}
	now := time.Now()
	wasUpdate := task.LastDigest == "" || task.LastDigest != img.ManifestDigest
	s.store.PatchTask(j.taskID, func(t *model.Task) {
		t.LastDigest = img.ManifestDigest
		t.LastConfigDigest = img.ConfigDigest
		t.LastCheckedAt = &now
		t.LastSyncedAt = &now
		t.LastError = ""
		t.UpdateAvailable = false
		t.RemoteDigest = img.ManifestDigest
		t.LatestTar = name
		t.LatestTarSize = size
		t.Status = model.TaskStatusIdle
		t.LastRunID = j.runID
	})
	s.store.PutTarFile(store.TarFile{
		Name:     name,
		Size:     size,
		ModTime:  modTime,
		TaskID:   task.ID,
		TaskName: task.Name,
		Image:    ref.String(),
		Digest:   img.ManifestDigest,
	})
	s.finishRun(j.runID, model.RunStatusSuccess, "", func(r *model.Run) {
		r.RemoteDigest = img.ManifestDigest
		r.PreviousDigest = task.LastDigest
		r.ConfigDigest = img.ConfigDigest
		r.UpdateAvailable = wasUpdate
		r.TarName = name
		r.TarSize = size
		r.Layers = len(img.Layers)
		r.BytesDownloaded = stats.BytesDownloaded
		r.BlobsReused = stats.BlobsReused
	}, now)

	keep := task.KeepVersions
	if keep <= 0 {
		keep = 3
	}
	if removed := s.store.PruneTaskTars(task.ID, keep); len(removed) > 0 {
		s.logRun(j.runID, model.LevelInfo, "按保留策略删除旧 tar：%s", strings.Join(removed, ", "))
	}
	s.logRun(j.runID, model.LevelInfo, "同步完成：%s（%d 字节）", name, size)
}

// executeCheck resolves the remote digest for a scheduled/manual check run. It
// returns true when the caller should start an automatic sync.
func (s *Scheduler) executeCheck(j *job) bool {
	ctx, cancel := s.newRunContext(j.runID)
	defer s.releaseRunContext(j.runID, cancel)

	task, ok := s.store.GetTask(j.taskID)
	if !ok {
		s.finishRun(j.runID, model.RunStatusCanceled, "任务已删除", nil, time.Now())
		return false
	}
	if j.trigger == model.TriggerSchedule && !task.Enabled {
		s.finishRun(j.runID, model.RunStatusCanceled, "任务已禁用，跳过检查", nil, time.Now())
		return false
	}
	s.startRun(j, task, model.PhaseResolving)

	ref, err := registry.Parse(task.Image)
	if err != nil {
		if s.canceled(ctx, j) {
			return false
		}
		s.failRun(j.runID, j.taskID, "解析镜像引用失败", err)
		return false
	}
	platform, err := s.platformFor(task)
	if err != nil {
		if s.canceled(ctx, j) {
			return false
		}
		s.failRun(j.runID, j.taskID, "解析平台失败", err)
		return false
	}
	s.updateRun(j.runID, func(r *model.Run) {
		r.Image = ref.String()
		r.Platform = platform
	})

	res, err := s.performCheck(ctx, j.runID, task, ref, platform)
	if err != nil {
		if s.canceled(ctx, j) {
			return false
		}
		s.failRun(j.runID, j.taskID, "检查更新失败", err)
		return false
	}
	if j.trigger == model.TriggerSchedule && task.AutoSync && res.UpdateAvailable {
		s.logRun(j.runID, model.LevelInfo, "检测到新版本，触发自动同步")
		return true
	}
	return false
}

// performCheck resolves the remote digest, records the run result and updates
// the task's check timestamps. The run must already exist.
func (s *Scheduler) performCheck(ctx context.Context, runID string, task model.Task, ref registry.Ref, platform string) (CheckResult, error) {
	client := s.clientFor(ref.Registry)
	s.logRun(runID, model.LevelInfo, "开始解析镜像 %s（平台 %s）", ref.String(), platform)
	resolveCtx, cancel := context.WithTimeout(ctx, runResolveTimeout)
	img, err := client.Resolve(resolveCtx, ref, platform)
	cancel()
	if err != nil {
		return CheckResult{}, fmt.Errorf("解析远端镜像 %s 失败: %w", ref.String(), err)
	}

	changed := task.LastDigest == "" || task.LastDigest != img.ManifestDigest
	now := time.Now()
	res := CheckResult{
		UpdateAvailable: changed,
		RemoteDigest:    img.ManifestDigest,
		CurrentDigest:   task.LastDigest,
		Image:           ref.String(),
		Platform:        platform,
	}
	if changed {
		s.logRun(runID, model.LevelInfo, "发现更新：远端 %s，本地 %s", shortDigest(img.ManifestDigest), shortDigest(task.LastDigest))
	} else {
		s.logRun(runID, model.LevelInfo, "镜像无变化（digest %s）", shortDigest(img.ManifestDigest))
	}
	s.store.PatchTask(task.ID, func(t *model.Task) {
		t.LastCheckedAt = &now
		t.RemoteDigest = img.ManifestDigest
		t.UpdateAvailable = changed
		t.LastRunID = runID
		if !changed {
			t.LastError = ""
		}
		// The check run itself is over, so the task must not stay "running"
		// (a follow-up sync, if any, re-queues it). Paused is sticky.
		if t.Status != model.TaskStatusPaused {
			t.Status = model.TaskStatusIdle
		}
	})
	s.finishRun(runID, model.RunStatusSuccess, "", func(r *model.Run) {
		r.RemoteDigest = img.ManifestDigest
		r.PreviousDigest = task.LastDigest
		r.ConfigDigest = img.ConfigDigest
		r.UpdateAvailable = changed
		r.Layers = len(img.Layers)
	}, now)
	return res, nil
}

// canceled marks the run canceled when its context was aborted (shutdown or
// task deletion) and reports whether that happened.
func (s *Scheduler) canceled(ctx context.Context, j *job) bool {
	if ctx.Err() == nil {
		return false
	}
	s.finishRun(j.runID, model.RunStatusCanceled, "运行已取消", nil, time.Now())
	s.store.PatchTask(j.taskID, func(t *model.Task) {
		if t.Status != model.TaskStatusPaused {
			t.Status = model.TaskStatusIdle
		}
	})
	return true
}

// failRun records an error on both the run and its task.
func (s *Scheduler) failRun(runID, taskID, msg string, cause error) {
	text := msg
	if cause != nil {
		text = fmt.Sprintf("%s: %v", msg, cause)
	}
	s.logRun(runID, model.LevelError, "%s", text)
	now := time.Now()
	s.finishRun(runID, model.RunStatusError, text, nil, now)
	s.store.PatchTask(taskID, func(t *model.Task) {
		t.LastError = text
		if !t.Enabled {
			t.Status = model.TaskStatusPaused
		} else {
			t.Status = model.TaskStatusError
		}
	})
}

// platformFor normalises a task platform, falling back to the global default.
func (s *Scheduler) platformFor(task model.Task) (string, error) {
	raw := strings.TrimSpace(task.Platform)
	if raw == "" {
		raw = s.store.SnapshotSettings().DefaultPlatform
	}
	if strings.TrimSpace(raw) == "" {
		raw = "linux/amd64"
	}
	p, err := registry.ParsePlatform(raw)
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// displayPlatform is the best effort platform for a newly created run.
func (s *Scheduler) displayPlatform(task model.Task) string {
	if p, err := s.platformFor(task); err == nil {
		return p
	}
	return strings.TrimSpace(task.Platform)
}

// clientFor returns a cached registry client for the given host.
func (s *Scheduler) clientFor(host string) RegistryAPI {
	if s.clientFactory != nil {
		return s.clientFactory(host)
	}
	s.mu.Lock()
	if c, ok := s.clients[host]; ok {
		s.mu.Unlock()
		return c
	}
	s.mu.Unlock()

	st := s.store.SnapshotSettings()
	opts := registry.Options{
		MaxConcurrentDownloads: st.MaxConcurrentDownloads,
		UserAgent:              "docker-sync",
		ProxyURL:               st.ProxyURL,
		NoProxy:                st.NoProxy,
	}
	if auth, ok := s.store.RegistryAuthFor(host); ok {
		opts.Username = auth.Username
		opts.Password = auth.Password
		opts.Insecure = auth.Insecure
		// A per-registry proxy overrides the global one.
		if auth.ProxyURL != "" {
			opts.ProxyURL = auth.ProxyURL
		}
	}
	client := registry.New(opts)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.clients[host]; ok {
		return existing
	}
	s.clients[host] = client
	return client
}

// shortDigest trims a digest for human readable log lines.
func shortDigest(digest string) string {
	if digest == "" {
		return "(空)"
	}
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// removeQuietly deletes a partially written artifact.
func removeQuietly(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The caller is already reporting a failure; keep this best effort.
		_ = err
	}
}
