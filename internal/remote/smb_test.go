package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"docker-sync/internal/model"
)

func validSMB() model.SMBSettings {
	return model.SMBSettings{Enabled: true, Share: "//nas/backups", Username: "backup", Directory: "images/daily"}
}

func TestValidateSMB(t *testing.T) {
	if err := Validate(validSMB()); err != nil {
		t.Fatal(err)
	}
	withPort := validSMB()
	withPort.Share = "//nas:445/backups"
	if err := Validate(withPort); err != nil {
		t.Fatalf("share with port rejected: %v", err)
	}
	disabled := model.SMBSettings{Enabled: false, Share: "nonsense"}
	if err := Validate(disabled); err != nil {
		t.Fatalf("disabled config must be accepted: %v", err)
	}
	for _, mutate := range []func(*model.SMBSettings){
		func(c *model.SMBSettings) { c.Share = "//nas/share/path" },
		func(c *model.SMBSettings) { c.Share = "nas/backups" },
		func(c *model.SMBSettings) { c.Directory = "../escape" },
		func(c *model.SMBSettings) { c.Directory = "/absolute" },
		func(c *model.SMBSettings) { c.Directory = "x; del *" },
		func(c *model.SMBSettings) { c.Directory = "x\"" },
		func(c *model.SMBSettings) { c.Directory = `a\b` },
		func(c *model.SMBSettings) { c.Password = "secret\nusername=bad" },
		func(c *model.SMBSettings) { c.Username = "  backup" },
		func(c *model.SMBSettings) { c.Username = "" },
	} {
		c := validSMB()
		mutate(&c)
		if Validate(c) == nil {
			t.Errorf("accepted invalid config: %+v", c)
		}
	}
}

type fakeSession struct {
	created   []string
	renamed   [][2]string
	removed   []string
	createErr error
	renameErr error
	writeErr  error
	closed    bool
	body      bytes.Buffer
}

func (s *fakeSession) Create(name string) (io.WriteCloser, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.created = append(s.created, name)
	return &fakeFile{session: s}, nil
}

func (s *fakeSession) Rename(oldName, newName string) error {
	s.renamed = append(s.renamed, [2]string{oldName, newName})
	return s.renameErr
}

func (s *fakeSession) Remove(name string) error {
	s.removed = append(s.removed, name)
	return nil
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

// fakeFile 只允许第一次写入成功，用来模拟传输中途断链。
type fakeFile struct {
	session *fakeSession
	written int
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.session.writeErr != nil && f.written > 0 {
		return 0, f.session.writeErr
	}
	f.written += len(p)
	return f.session.body.Write(p)
}

func (f *fakeFile) Close() error { return nil }

func newArchive(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.tar")
	// 比 chunkSize 大一点，保证至少发生两次写入，便于验证中途失败。
	payload := bytes.Repeat([]byte("docker-sync"), (chunkSize/len("docker-sync"))+64)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dialReturning(sess *fakeSession) dialFunc {
	return func(context.Context, model.SMBSettings) (shareSession, error) { return sess, nil }
}

func TestUploadPublishesViaTempName(t *testing.T) {
	path := newArchive(t)
	sess := &fakeSession{}
	var dialed model.SMBSettings
	var progressCalls int
	err := upload(context.Background(), validSMB(), path, func(_ context.Context, cfg model.SMBSettings) (shareSession, error) {
		dialed = cfg
		return sess, nil
	}, func(copied, total int64) {
		progressCalls++
		if total <= 0 || copied > total {
			t.Fatalf("bad progress %d/%d", copied, total)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialed.Share != validSMB().Share {
		t.Fatalf("dial got %+v", dialed)
	}
	if len(sess.created) != 1 || !strings.HasPrefix(sess.created[0], ".docker-sync-") || !strings.HasSuffix(sess.created[0], ".part") {
		t.Fatalf("temp file not used: %v", sess.created)
	}
	if len(sess.renamed) != 1 || sess.renamed[0] != [2]string{sess.created[0], "image.tar"} {
		t.Fatalf("rename sequence wrong: %v", sess.renamed)
	}
	if !sess.closed {
		t.Fatal("session not closed")
	}
	if progressCalls == 0 {
		t.Fatal("progress was never reported")
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sess.body.Bytes(), want) {
		t.Fatalf("remote content mismatch: %d vs %d bytes", sess.body.Len(), len(want))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("local archive removed")
	}
}

func TestUploadCleansPartialOnFailure(t *testing.T) {
	path := newArchive(t)
	cfg := validSMB()

	createFail := &fakeSession{createErr: errors.New("access denied")}
	if err := upload(context.Background(), cfg, path, dialReturning(createFail), nil); err == nil {
		t.Fatal("create failure must surface")
	}

	writeFail := &fakeSession{writeErr: errors.New("connection reset")}
	err := upload(context.Background(), cfg, path, dialReturning(writeFail), nil)
	if err == nil {
		t.Fatal("write failure must surface")
	}
	if !strings.Contains(err.Error(), "中断") {
		t.Fatalf("unexpected message: %v", err)
	}
	if !contains(writeFail.removed, writeFail.created[0]) {
		t.Fatalf("partial not cleaned up: %v", writeFail.removed)
	}
	if len(writeFail.renamed) != 0 {
		t.Fatalf("unexpected rename: %v", writeFail.renamed)
	}

	renameFail := &fakeSession{renameErr: errors.New("target exists")}
	if err := upload(context.Background(), cfg, path, dialReturning(renameFail), nil); err == nil {
		t.Fatal("rename failure must surface")
	}
	if len(renameFail.renamed) != 1 {
		t.Fatalf("rename not attempted: %v", renameFail.renamed)
	}
	if !contains(renameFail.removed, renameFail.created[0]) {
		t.Fatalf("partial not cleaned up after rename failure: %v", renameFail.removed)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatal("local archive removed")
	}
}

func TestUploadDisabledDoesNotDial(t *testing.T) {
	path := newArchive(t)
	cfg := validSMB()
	cfg.Enabled = false
	err := upload(context.Background(), cfg, path, func(context.Context, model.SMBSettings) (shareSession, error) {
		t.Fatal("dial must not happen when SMB is disabled")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestUploadRejectsBadInputs(t *testing.T) {
	path := newArchive(t)
	dial := func(context.Context, model.SMBSettings) (shareSession, error) {
		t.Fatal("dial must not happen")
		return nil, nil
	}
	if err := upload(context.Background(), validSMB(), filepath.Join(t.TempDir(), "missing.tar"), dial, nil); err == nil {
		t.Fatal("missing archive must fail")
	}
	badName := filepath.Join(t.TempDir(), "quoted\".tar")
	if err := os.WriteFile(badName, []byte("x"), 0o600); err != nil {
		t.Logf("platform rejected the file name, skipping that case: %v", err)
	} else if err := upload(context.Background(), validSMB(), badName, dial, nil); err == nil {
		t.Fatal("unsafe filename must fail")
	}
	notes := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notes, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upload(context.Background(), validSMB(), notes, dial, nil); err == nil {
		t.Fatal("non-tar archive must fail")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("local archive removed")
	}
}

func TestUploadStopsOnCanceledContext(t *testing.T) {
	path := newArchive(t)
	sess := &fakeSession{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := upload(ctx, validSMB(), path, dialReturning(sess), nil)
	if err == nil {
		t.Fatal("canceled context must abort upload")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSplitShareAndRemoteDir(t *testing.T) {
	host, share, err := splitShare("//nas.example.com/backups")
	if err != nil || host != "nas.example.com" || share != "backups" {
		t.Fatalf("split = %q %q %v", host, share, err)
	}
	if _, _, err := splitShare("//nas"); err == nil {
		t.Fatal("missing share must fail")
	}
	if got := remoteDir("images/daily"); got != `images\daily` {
		t.Fatalf("remoteDir = %q", got)
	}
	if got := remoteDir("/"); got != "" {
		t.Fatalf("remoteDir root = %q", got)
	}
	if got := joinRemote("", "a.tar"); got != "a.tar" {
		t.Fatalf("joinRemote = %q", got)
	}
	if got := joinRemote(`images`, "a.tar"); got != `images\a.tar` {
		t.Fatalf("joinRemote = %q", got)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
func TestHostPort(t *testing.T) {
	if got := hostPort("nas"); got != "nas:445" {
		t.Fatalf("hostPort = %q", got)
	}
	if got := hostPort("10.0.0.5"); got != "10.0.0.5:445" {
		t.Fatalf("hostPort = %q", got)
	}
	if got := hostPort("nas:1445"); got != "nas:1445" {
		t.Fatalf("hostPort = %q", got)
	}
	if got := hostPort("[::1]"); got != "[::1]:445" {
		t.Fatalf("hostPort = %q", got)
	}
}
