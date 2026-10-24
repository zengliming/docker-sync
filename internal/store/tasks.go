package store

import (
	"fmt"
	"sort"
	"time"

	"docker-sync/internal/model"
)

// ListTasks returns all tasks sorted by creation time descending.
func (s *Store) ListTasks() []model.Task {
	s.mu.RLock()
	out := make([]model.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, *copyTask(t))
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// GetTask returns a copy of the task with the given id.
func (s *Store) GetTask(id string) (model.Task, bool) {
	s.mu.RLock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.RUnlock()
		return model.Task{}, false
	}
	cp := *copyTask(t)
	s.mu.RUnlock()
	return cp, true
}

// CreateTask adds a new task, assigning an id and timestamps when missing.
// The returned task is a snapshot of the stored value.
func (s *Store) CreateTask(t model.Task) (model.Task, error) {
	s.mu.Lock()
	if t.ID == "" {
		t.ID = newID("t_")
	}
	if _, exists := s.tasks[t.ID]; exists {
		s.mu.Unlock()
		return model.Task{}, fmt.Errorf("store: task %s already exists", t.ID)
	}
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = model.TaskStatusIdle
	}
	stored := copyTask(&t)
	s.tasks[t.ID] = stored
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "task", ID: t.ID, Message: "created"})
	return *copyTask(stored), nil
}

// UpdateTask replaces the editable and runtime fields of an existing task,
// preserving its id and creation time. The caller is expected to build the
// full task (usually by patching the current value).
func (s *Store) UpdateTask(t model.Task) (model.Task, error) {
	s.mu.Lock()
	old, ok := s.tasks[t.ID]
	if !ok {
		s.mu.Unlock()
		return model.Task{}, fmt.Errorf("store: task %s not found", t.ID)
	}
	t.CreatedAt = old.CreatedAt
	t.UpdatedAt = time.Now()
	stored := copyTask(&t)
	s.tasks[t.ID] = stored
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "task", ID: t.ID, Message: "updated"})
	return *copyTask(stored), nil
}

// PatchTask atomically mutates the stored task through fn and returns the new
// snapshot. It reports false when the task does not exist.
func (s *Store) PatchTask(id string, fn func(*model.Task)) (model.Task, bool) {
	s.mu.Lock()
	cur, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return model.Task{}, false
	}
	next := copyTask(cur)
	fn(next)
	next.ID = id
	next.CreatedAt = cur.CreatedAt
	next.UpdatedAt = time.Now()
	s.tasks[id] = next
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "task", ID: id, Message: "updated"})
	return *copyTask(next), true
}

// DeleteTask removes a task and every tar artifact it produced. It reports
// false when the task does not exist.
func (s *Store) DeleteTask(id string) bool {
	s.mu.Lock()
	if _, ok := s.tasks[id]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.tasks, id)
	names := make([]string, 0, 4)
	for name, f := range s.files {
		if f.TaskID == id {
			names = append(names, name)
			delete(s.files, name)
		}
	}
	s.mu.Unlock()

	for _, name := range names {
		if err := removeFile(s.tarPath(name)); err != nil {
			s.log.Warn("cannot remove tar artifact of deleted task", "task", id, "file", name, "err", err)
		}
	}
	s.markDirty()
	s.emit(model.Event{Type: "task", ID: id, Message: "deleted"})
	if len(names) > 0 {
		s.emit(model.Event{Type: "file", Message: "deleted"})
	}
	return true
}

// sanitizeTask clamps values that came from an untrusted (or older) state file
// into the documented ranges.
func sanitizeTask(t *model.Task) {
	if t.ScheduleType != model.ScheduleCron {
		t.ScheduleType = model.ScheduleInterval
	}
	if t.IntervalMinutes <= 0 {
		t.IntervalMinutes = 60
	}
	if t.KeepVersions <= 0 {
		t.KeepVersions = 3
	}
	if t.KeepVersions > 50 {
		t.KeepVersions = 50
	}
	if t.Status == "" {
		t.Status = model.TaskStatusIdle
	}
	if t.ScheduleType == model.ScheduleInterval {
		t.Cron = ""
	}
}

func copyTask(t *model.Task) *model.Task {
	cp := *t
	cp.LastCheckedAt = copyTime(t.LastCheckedAt)
	cp.LastSyncedAt = copyTime(t.LastSyncedAt)
	cp.NextRunAt = copyTime(t.NextRunAt)
	return &cp
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
