package scheduler

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"docker-sync/internal/model"
	"docker-sync/internal/registry"
	"docker-sync/internal/store"
)

// TestRepoTagForTagsTarsWithConcreteVersion guards the tag written into the tar:
// a latest-following task must load as the concrete version, never as latest.
func TestRepoTagForTagsTarsWithConcreteVersion(t *testing.T) {
	digestRef, err := registry.Parse("busybox@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		name  string
		task  model.Task
		image string
		want  string
	}{
		{
			name:  "followed latest is tagged with the concrete version",
			task:  model.Task{Image: "docker.io/library/alpine:latest", IgnoreLatest: true},
			image: "docker.io/library/alpine:3.24.3",
			want:  "docker.io/library/alpine:3.24.3",
		},
		{
			name:  "pinned tag keeps its own reference",
			task:  model.Task{Image: "docker.io/library/alpine:3.20"},
			image: "docker.io/library/alpine:3.20",
			want:  "docker.io/library/alpine:3.20",
		},
		{
			name:  "digest reference is left untouched",
			task:  model.Task{Image: digestRef.String()},
			image: digestRef.String(),
			want:  digestRef.String(),
		},
	}
	for _, tc := range cases {
		ref, err := registry.Parse(tc.image)
		if err != nil {
			t.Fatalf("%s: Parse: %v", tc.name, err)
		}
		if got := repoTagFor(tc.task, ref); got != tc.want {
			t.Errorf("%s: repoTagFor() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestIgnoredLatestTagMatches(t *testing.T) {
	const digestRef = "alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	cases := []struct {
		name   string
		image  string
		ignore bool
		want   bool
	}{
		{"latest tag", "alpine:latest", true, true},
		{"implicit latest", "alpine", true, true},
		{"explicit docker hub", "docker.io/library/alpine:latest", true, true},
		{"pinned tag", "alpine:3.20", true, false},
		{"digest reference", digestRef, true, false},
		{"switch off", "alpine:latest", false, false},
	}
	for _, tc := range cases {
		task := model.Task{Image: tc.image, IgnoreLatest: tc.ignore}
		if got := ignoredLatestTag(task); got != tc.want {
			t.Errorf("%s: ignoredLatestTag(%q) = %v, want %v", tc.name, tc.image, got, tc.want)
		}
	}
}

func TestPickNewestTag(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		want    string
		wantErr string
	}{
		{
			name: "semver wins over latest and flavours",
			tags: []string{"latest", "3.20-alpine", "3.19.9", "3.20", "3.20.1", "stable"},
			want: "3.20.1",
		},
		{
			name: "leading v accepted",
			tags: []string{"v1.9.0", "v1.10.0", "v1.2.3"},
			want: "v1.10.0",
		},
		{
			name: "prereleases and word tags ignored",
			tags: []string{"latest", "edge", "2.0.0-rc1", "1.4.2"},
			want: "1.4.2",
		},
		{
			name: "date stamped snapshots lose to the release line",
			tags: []string{"latest", "20260805", "3.20.1", "3.20", "20260731"},
			want: "3.20.1",
		},
		{
			name: "single component versions still work alone",
			tags: []string{"latest", "20260805", "20260731"},
			want: "20260805",
		},
		{
			name: "equal versions keep the first seen",
			tags: []string{"3.20", "3.20.0"},
			want: "3.20",
		},
		{
			name:    "no version tags",
			tags:    []string{"latest", "stable", "edge"},
			wantErr: "没有形如 1.2.3 的版本标签",
		},
		{name: "empty list", wantErr: "没有形如 1.2.3 的版本标签"},
	}
	for _, tc := range cases {
		got, err := pickNewestTag(tc.tags)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: pickNewestTag(%v) = %q, want %q", tc.name, tc.tags, got, tc.want)
		}
	}
}

// seedArchive gives the task a finished tar for the fake digest so the next sync
// takes the digest-skip path: the resolution still runs, but no packing is
// needed (the fake registry does not produce blobs).
func seedArchive(t *testing.T, env *schedEnv, taskID string) string {
	t.Helper()
	const tarName = "library_alpine_3.20.1_linux_amd64_111111111111.tar"
	if err := os.WriteFile(env.store.TarPath(tarName), []byte("fake tar"), 0o644); err != nil {
		t.Fatalf("write tar: %v", err)
	}
	env.store.PutTarFile(store.TarFile{Name: tarName, Size: 8, ModTime: time.Now(), TaskID: taskID})
	env.store.PatchTask(taskID, func(task *model.Task) {
		task.LastDigest = testDigest
		task.LatestTar = tarName
		task.LatestTarSize = 8
	})
	return tarName
}

// TestIgnoredLatestSyncsNewestVersion is the core behaviour: a task pointing at
// latest syncs the newest concrete version instead of the moving tag.
func TestIgnoredLatestSyncsNewestVersion(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tags = []string{"latest", "3.19.9", "3.20", "3.20.1", "edge"}
	created := env.createTask(func(task *model.Task) {
		task.Image = "docker.io/library/alpine:latest"
		task.IgnoreLatest = true
	})
	seedArchive(t, env, created.ID)

	run, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusSkipped {
		t.Fatalf("run status = %s error %q, want skipped", done.Status, done.Error)
	}
	if got := env.fake.tagsResolved(); len(got) != 1 || got[0] != "3.20.1" {
		t.Fatalf("resolved tags = %v, want [3.20.1]", got)
	}
	if !strings.Contains(done.Image, "alpine:3.20.1") {
		t.Fatalf("run image = %q, want the concrete version", done.Image)
	}
	task, _ := env.store.GetTask(created.ID)
	if task.ResolvedTag != "3.20.1" {
		t.Fatalf("task resolvedTag = %q, want 3.20.1", task.ResolvedTag)
	}
}

// TestIgnoredLatestScheduledCheckFollowsNewestVersion proves scheduled checks run
// (no more skipping) and follow the concrete version.
func TestIgnoredLatestScheduledCheckFollowsNewestVersion(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tags = []string{"latest", "3.20.1", "3.21.0"}
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})

	run, err := env.sched.StartCheck(created.ID, model.TriggerSchedule)
	if err != nil {
		t.Fatalf("StartCheck: %v", err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusSuccess {
		t.Fatalf("run status = %s error %q, want success", done.Status, done.Error)
	}
	if done.Trigger != model.TriggerSchedule {
		t.Fatalf("trigger = %s", done.Trigger)
	}
	if got := env.fake.tagsResolved(); len(got) == 0 {
		t.Fatalf("no resolve calls, want the concrete version")
	} else {
		for i, tag := range got {
			if tag != "3.21.0" {
				t.Fatalf("resolve %d used tag %q, want 3.21.0", i, tag)
			}
		}
	}
	task, _ := env.store.GetTask(created.ID)
	if task.ResolvedTag != "3.21.0" {
		t.Fatalf("task resolvedTag = %q, want 3.21.0", task.ResolvedTag)
	}
}

// TestIgnoredLatestFailsWhenUpstreamHasNoVersions keeps the failure readable
// when a repository only publishes moving or flavour tags.
func TestIgnoredLatestFailsWhenUpstreamHasNoVersions(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tags = []string{"latest", "stable"}
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})

	run, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusError {
		t.Fatalf("run status = %s, want error", done.Status)
	}
	if !strings.Contains(done.Error, "没有形如 1.2.3 的版本标签") {
		t.Fatalf("run error = %q", done.Error)
	}
	if env.fake.downloadCount() != 0 {
		t.Fatalf("downloads = %d, want 0", env.fake.downloadCount())
	}
	task, _ := env.store.GetTask(created.ID)
	if !strings.Contains(task.LastError, "没有形如 1.2.3 的版本标签") {
		t.Fatalf("task lastError = %q", task.LastError)
	}
}

// TestIgnoredLatestTaskKeepsSchedule: ignoring latest no longer removes the task
// from the schedule, it only changes which tag is fetched.
func TestIgnoredLatestTaskKeepsSchedule(t *testing.T) {
	env := newSchedEnv(t)
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})
	env.sched.TaskChanged(created.ID)
	task, _ := env.store.GetTask(created.ID)
	if task.NextRunAt == nil {
		t.Fatalf("nextRunAt = nil, want a scheduled check")
	}
}

// TestPinnedTagIgnoresResolution makes sure tasks with an explicit tag leave the
// tag list alone.
func TestPinnedTagIgnoresResolution(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tags = []string{"latest", "3.20.1"}
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:3.20"
		task.IgnoreLatest = true
	})

	run, err := env.sched.StartCheck(created.ID, model.TriggerManual)
	if err != nil {
		t.Fatalf("StartCheck: %v", err)
	}
	if done := env.waitRunDone(run.ID); done.Status != model.RunStatusSuccess {
		t.Fatalf("run status = %s error %q", done.Status, done.Error)
	}
	if env.fake.listCount() != 0 {
		t.Fatalf("ListTags called %d times for a pinned tag, want 0", env.fake.listCount())
	}
	task, _ := env.store.GetTask(created.ID)
	if task.ResolvedTag != "" {
		t.Fatalf("resolvedTag = %q, want empty for a pinned tag", task.ResolvedTag)
	}
}

// TestCheckNowFollowsNewestVersion covers the synchronous manual check path.
func TestCheckNowFollowsNewestVersion(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tags = []string{"latest", "3.19.9", "3.20.1"}
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})

	res, err := env.sched.CheckNow(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("CheckNow: %v", err)
	}
	if !strings.Contains(res.Image, "alpine:3.20.1") {
		t.Fatalf("checked image = %q, want the concrete version", res.Image)
	}
	task, _ := env.store.GetTask(created.ID)
	if task.ResolvedTag != "3.20.1" {
		t.Fatalf("resolvedTag = %q, want 3.20.1", task.ResolvedTag)
	}
}

// TestLatestResolutionErrorIsReported surfaces a failing tags/list call.
func TestLatestResolutionErrorIsReported(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tagsErr = errors.New("registry: unauthorized")
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})

	run, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusError {
		t.Fatalf("run status = %s, want error", done.Status)
	}
	if !strings.Contains(done.Error, "列出") || !strings.Contains(done.Error, "unauthorized") {
		t.Fatalf("run error = %q", done.Error)
	}
}

// TestIgnoredLatestResolvesOnEverySync proves the newest version is looked up on
// every run, so a fresh upstream release is picked up without touching the task.
func TestIgnoredLatestResolvesOnEverySync(t *testing.T) {
	env := newSchedEnv(t)
	env.fake.tags = []string{"latest", "3.20.1"}
	created := env.createTask(func(task *model.Task) {
		task.Image = "alpine:latest"
		task.IgnoreLatest = true
	})
	seedArchive(t, env, created.ID)

	first, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatalf("first TriggerSync: %v", err)
	}
	if done := env.waitRunDone(first.ID); done.Status != model.RunStatusSkipped {
		t.Fatalf("first run status = %s error %q", done.Status, done.Error)
	}

	// A new release appears upstream: the next run must resolve again and see it.
	env.fake.mu.Lock()
	env.fake.tags = []string{"latest", "3.20.1", "3.21.0"}
	env.fake.digest = "sha256:" + strings.Repeat("9", 64)
	env.fake.mu.Unlock()

	second, err := env.sched.TriggerSync(created.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatalf("second TriggerSync: %v", err)
	}
	if done := env.waitRunDone(second.ID); done.Status == model.RunStatusSkipped && done.RemoteDigest == testDigest {
		t.Fatalf("second run reused the old digest, want the new release")
	}
	if got := env.fake.tagsResolved(); len(got) != 2 || got[0] != "3.20.1" || got[1] != "3.21.0" {
		t.Fatalf("resolved tags = %v, want [3.20.1 3.21.0]", got)
	}
	task, _ := env.store.GetTask(created.ID)
	if task.ResolvedTag != "3.21.0" {
		t.Fatalf("resolvedTag = %q, want 3.21.0", task.ResolvedTag)
	}
}
