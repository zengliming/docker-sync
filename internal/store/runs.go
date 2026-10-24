package store

import (
	"fmt"
	"sort"
	"time"

	"docker-sync/internal/model"
)

// CreateRun stores a new run record. Missing ids/timestamps/status defaults
// are filled in. The returned run is a snapshot.
func (s *Store) CreateRun(r model.Run) (model.Run, error) {
	s.mu.Lock()
	if r.ID == "" {
		r.ID = newID("r_")
	}
	if _, exists := s.runs[r.ID]; exists {
		s.mu.Unlock()
		return model.Run{}, fmt.Errorf("store: run %s already exists", r.ID)
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	if r.Status == "" {
		r.Status = model.RunStatusRunning
	}
	if r.Phase == "" {
		r.Phase = model.PhaseQueued
	}
	trimLogs(&r)
	stored := copyRun(&r)
	s.runs[r.ID] = stored
	s.pruneRunsLocked()
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "run", ID: r.ID, Message: "created"})
	return *copyRun(stored), nil
}

// GetRun returns one run including its log lines.
func (s *Store) GetRun(id string) (model.Run, bool) {
	s.mu.RLock()
	r, ok := s.runs[id]
	if !ok {
		s.mu.RUnlock()
		return model.Run{}, false
	}
	cp := *copyRun(r)
	s.mu.RUnlock()
	return cp, true
}

// ListRuns returns up to limit run records (without logs), newest first. When
// taskID is non-empty only that task's runs are returned.
func (s *Store) ListRuns(limit int, taskID string) []model.Run {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	s.mu.RLock()
	out := make([]model.Run, 0, len(s.runs))
	for _, r := range s.runs {
		if taskID != "" && r.TaskID != taskID {
			continue
		}
		cp := *r
		cp.Logs = nil
		out = append(out, cp)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// PatchRun mutates a stored run through fn and returns the new snapshot.
func (s *Store) PatchRun(id string, fn func(*model.Run)) (model.Run, bool) {
	s.mu.Lock()
	cur, ok := s.runs[id]
	if !ok {
		s.mu.Unlock()
		return model.Run{}, false
	}
	next := copyRun(cur)
	fn(next)
	next.ID = id
	if next.StartedAt.IsZero() {
		next.StartedAt = cur.StartedAt
	}
	trimLogs(next)
	s.runs[id] = next
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "run", ID: id, Message: "updated"})
	return *copyRun(next), true
}

// AppendRunLog appends one log line to a run, keeping at most MaxRunLogs
// lines (oldest dropped). It returns false when the run does not exist.
func (s *Store) AppendRunLog(id, level, message string) bool {
	if level == "" {
		level = model.LevelInfo
	}
	line := model.LogLine{Time: time.Now(), Level: level, Message: message}
	s.mu.Lock()
	r, ok := s.runs[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	r.Logs = append(r.Logs, line)
	if len(r.Logs) > MaxRunLogs {
		r.Logs = append([]model.LogLine(nil), r.Logs[len(r.Logs)-MaxRunLogs:]...)
	}
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "log", ID: id, Message: message})
	return true
}

// pruneRunsLocked drops the oldest finished runs while the history exceeds
// maxRuns. The caller must hold the write lock.
func (s *Store) pruneRunsLocked() {
	if len(s.runs) <= maxRuns {
		return
	}
	finished := make([]*model.Run, 0, len(s.runs))
	for _, r := range s.runs {
		if r.Status != model.RunStatusRunning {
			finished = append(finished, r)
		}
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].StartedAt.Before(finished[j].StartedAt) })
	for _, r := range finished {
		if len(s.runs) <= maxRuns {
			break
		}
		delete(s.runs, r.ID)
	}
}

func trimLogs(r *model.Run) {
	if len(r.Logs) > MaxRunLogs {
		r.Logs = append([]model.LogLine(nil), r.Logs[len(r.Logs)-MaxRunLogs:]...)
	}
}

// RunningRuns returns every run still marked as running (without logs). It is
// used on shutdown to cancel runs that did not finish in time.
func (s *Store) RunningRuns() []model.Run {
	s.mu.RLock()
	out := make([]model.Run, 0, 4)
	for _, r := range s.runs {
		if r.Status != model.RunStatusRunning {
			continue
		}
		cp := *r
		cp.Logs = nil
		out = append(out, cp)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// RecoverAfterRestart repairs state left behind by an unclean shutdown: tasks
// stuck in queued/running return to idle and runs still marked running are
// marked canceled with a descriptive error.
func (s *Store) RecoverAfterRestart(now time.Time) (tasks, runs int) {
	s.mu.Lock()
	for _, t := range s.tasks {
		if t.Status == model.TaskStatusRunning || t.Status == model.TaskStatusQueued {
			t.Status = model.TaskStatusIdle
			t.UpdatedAt = now
			tasks++
		}
	}
	for _, r := range s.runs {
		if r.Status == model.RunStatusRunning {
			r.Status = model.RunStatusCanceled
			r.Phase = model.PhaseDone
			r.FinishedAt = &now
			if r.Error == "" {
				r.Error = "服务重启，运行已中断"
			}
			runs++
		}
	}
	s.mu.Unlock()
	if tasks > 0 || runs > 0 {
		s.markDirty()
	}
	return tasks, runs
}

func copyRun(r *model.Run) *model.Run {
	cp := *r
	cp.FinishedAt = copyTime(r.FinishedAt)
	if r.Logs != nil {
		cp.Logs = append([]model.LogLine(nil), r.Logs...)
	}
	return &cp
}
