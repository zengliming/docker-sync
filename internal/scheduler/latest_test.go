package scheduler

import (
	"errors"
	"testing"

	"docker-sync/internal/model"
)

func TestLatestTagIgnored(t *testing.T) {
	const digestRef = "alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	cases := []struct {
		name    string
		image   string
		trigger string
		ignore  bool
		want    bool
	}{
		{"latest tag", "alpine:latest", model.TriggerSchedule, true, true},
		{"implicit latest", "alpine", model.TriggerSchedule, true, true},
		{"explicit docker hub", "docker.io/library/alpine:latest", model.TriggerSchedule, true, true},
		{"pinned tag", "alpine:3.20", model.TriggerSchedule, true, false},
		{"digest reference", digestRef, model.TriggerSchedule, true, false},
		{"switch off", "alpine:latest", model.TriggerSchedule, false, false},
		{"manual sync", "alpine:latest", model.TriggerManual, true, false},
		{"manual check", "alpine:latest", model.TriggerCheck, true, false},
	}
	for _, tc := range cases {
		task := model.Task{Image: tc.image, IgnoreLatest: tc.ignore}
		if got := latestTagIgnored(task, tc.trigger); got != tc.want {
			t.Errorf("%s: latestTagIgnored(%q, %q) = %v, want %v", tc.name, tc.image, tc.trigger, got, tc.want)
		}
	}
}

func TestScheduledRunsSkipIgnoredLatest(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})

	if _, err := env.sched.StartCheck(created.ID, model.TriggerSchedule); !errors.Is(err, errIgnoredLatest) {
		t.Fatalf("scheduled StartCheck error = %v, want errIgnoredLatest", err)
	}
	if _, err := env.sched.TriggerSync(created.ID, model.TriggerSchedule, false); !errors.Is(err, errIgnoredLatest) {
		t.Fatalf("scheduled TriggerSync error = %v, want errIgnoredLatest", err)
	}
	if runs := env.store.ListRuns(10, ""); len(runs) != 0 {
		t.Fatalf("scheduled runs = %d, want 0 (skipped before creating a run)", len(runs))
	}
	if env.fake.downloadCount() != 0 {
		t.Fatalf("downloads = %d, want 0", env.fake.downloadCount())
	}
	task, ok := env.store.GetTask(created.ID)
	if !ok {
		t.Fatalf("task %s vanished", created.ID)
	}
	if task.Status == model.TaskStatusRunning || task.Status == model.TaskStatusQueued {
		t.Fatalf("task status = %s after a skipped schedule, want untouched", task.Status)
	}

	// Manual triggers still work for the same task.
	run, err := env.sched.StartCheck(created.ID, model.TriggerManual)
	if err != nil {
		t.Fatalf("manual StartCheck: %v", err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusSuccess {
		t.Fatalf("manual check status = %s error %q, want success", done.Status, done.Error)
	}
	task, _ = env.store.GetTask(created.ID)
	if !task.UpdateAvailable || task.RemoteDigest != testDigest {
		t.Fatalf("manual check did not record the remote digest: %+v", task)
	}
}

func TestIgnoredLatestTaskHasNoSchedule(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})
	env.sched.TaskChanged(created.ID)
	task, ok := env.store.GetTask(created.ID)
	if !ok {
		t.Fatalf("task %s vanished", created.ID)
	}
	if task.NextRunAt != nil {
		t.Fatalf("nextRunAt = %v, want nil for an ignored latest task", task.NextRunAt)
	}

	// Pinning a real tag schedules the task again.
	env.store.PatchTask(created.ID, func(t *model.Task) {
		t.Image = "docker.io/library/alpine:3.20"
	})
	env.sched.TaskChanged(created.ID)
	task, _ = env.store.GetTask(created.ID)
	if task.NextRunAt == nil {
		t.Fatalf("nextRunAt = nil after pinning a tag")
	}
}

func TestQueuedScheduleSkipsWhenTaskStartsIgnoringLatest(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
	})

	// The task opts out after the job was queued, so the executor must skip it.
	env.store.PatchTask(created.ID, func(task *model.Task) {
		task.IgnoreLatest = true
	})
	run, err := env.store.CreateRun(model.Run{
		TaskID:   created.ID,
		TaskName: created.Name,
		Image:    created.Image,
		Platform: created.Platform,
		Trigger:  model.TriggerSchedule,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	task, _ := env.store.GetTask(created.ID)
	if execute := env.sched.executeCheck(&job{kind: kindCheck, runID: run.ID, taskID: created.ID, trigger: model.TriggerSchedule}); execute {
		t.Fatalf("executeCheck asked for an automatic sync, want skip")
	}
	done, ok := env.store.GetRun(run.ID)
	if !ok {
		t.Fatalf("run %s vanished", run.ID)
	}
	if done.Status != model.RunStatusSkipped {
		t.Fatalf("run status = %s, want skipped", done.Status)
	}
	if env.fake.downloadCount() != 0 {
		t.Fatalf("downloads = %d, want 0", env.fake.downloadCount())
	}
	_ = task
}
