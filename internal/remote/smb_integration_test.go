package remote

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"docker-sync/internal/model"
)

// TestUploadToRealShare runs against a real SMB server when one is configured:
//
//	DS_TEST_SMB_SHARE=//nas.example/backups DS_TEST_SMB_USER=backup DS_TEST_SMB_PASS=secret \
//	DS_TEST_SMB_DIR=docker-images go test ./internal/remote/ -run TestUploadToRealShare -v
func TestUploadToRealShare(t *testing.T) {
	share := os.Getenv("DS_TEST_SMB_SHARE")
	if share == "" {
		t.Skip("set DS_TEST_SMB_SHARE/DS_TEST_SMB_USER/DS_TEST_SMB_PASS to run against a real SMB server")
	}
	cfg := model.SMBSettings{
		Enabled:   true,
		Share:     share,
		Directory: os.Getenv("DS_TEST_SMB_DIR"),
		Username:  os.Getenv("DS_TEST_SMB_USER"),
		Password:  os.Getenv("DS_TEST_SMB_PASS"),
		Domain:    os.Getenv("DS_TEST_SMB_DOMAIN"),
	}
	path := filepath.Join(t.TempDir(), "docker-sync-real-share.tar")
	if err := os.WriteFile(path, []byte("probe payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Upload(ctx, cfg, path); err != nil {
		t.Fatalf("upload: %v", err)
	}
	sess, err := dialShare(ctx, cfg)
	if err != nil {
		t.Fatalf("re-dial: %v", err)
	}
	defer sess.Close()
	if err := sess.Remove("docker-sync-real-share.tar"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
