package scheduler

import (
	"docker-sync/internal/model"
	"docker-sync/internal/store"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSMBFailureOnUnchangedDigestRetainsArchive(t *testing.T) {
	env := newSchedEnv(t)
	task := env.createTask(nil)
	name := "existing.tar"
	path := env.store.TarPath(name)
	if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	env.store.PutTarFile(store.TarFile{Name: name, Size: 7, ModTime: time.Now(), TaskID: task.ID})
	env.store.PatchTask(task.ID, func(t *model.Task) { t.LastDigest = testDigest; t.LatestTar = name })
	// Invalid internal configuration fails before contacting any SMB server.
	env.store.UpdateSettings(func(s *model.Settings) {
		s.SMB = model.SMBSettings{Enabled: true, Share: "invalid", Username: "backup"}
	})
	run, err := env.sched.TriggerSync(task.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatal(err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusError {
		t.Fatalf("SMB failure marked %s", done.Status)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("local archive lost")
	}
	if env.fake.downloadCount() != 0 {
		t.Fatal("existing archive triggered download")
	}
	got, _ := env.store.GetTask(task.ID)
	if got.LastError == "" || got.LastDigest != testDigest {
		t.Fatalf("unexpected task state: %+v", got)
	}
}

// TestSMBUnreachableHostReportsUploadError covers the real upload path: the
// share resolves to a closed port, so the pure Go SMB client fails fast and the
// run must report the upload error while keeping the local archive.
func TestSMBUnreachableHostReportsUploadError(t *testing.T) {
	env := newSchedEnv(t)
	task := env.createTask(nil)
	name := "existing.tar"
	path := env.store.TarPath(name)
	if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	env.store.PutTarFile(store.TarFile{Name: name, Size: 7, ModTime: time.Now(), TaskID: task.ID})
	env.store.PatchTask(task.ID, func(t *model.Task) { t.LastDigest = testDigest; t.LatestTar = name })
	env.store.UpdateSettings(func(s *model.Settings) {
		s.SMB = model.SMBSettings{Enabled: true, Share: "//127.0.0.1:1/backups", Directory: "docker", Username: "backup", Password: "secret"}
	})
	run, err := env.sched.TriggerSync(task.ID, model.TriggerManual, false)
	if err != nil {
		t.Fatal(err)
	}
	done := env.waitRunDone(run.ID)
	if done.Status != model.RunStatusError {
		t.Fatalf("SMB failure marked %s", done.Status)
	}
	if !strings.Contains(done.Error, "SMB 上传失败（本地 tar 已保留）") {
		t.Fatalf("error = %q", done.Error)
	}
	joined := ""
	for _, line := range done.Logs {
		joined += line.Message + "\n"
	}
	if !strings.Contains(joined, "SMB 正在上传 existing.tar") {
		t.Fatalf("missing upload log: %s", joined)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("local archive lost")
	}
}
