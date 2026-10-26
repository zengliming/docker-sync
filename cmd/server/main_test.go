package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"docker-sync/internal/tlscert"
)

func TestBuildVersion(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })

	cases := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "full sha is shortened", version: "0.1.2", commit: "1cdc7589a1b2c3d4e5f60718293a4b5c6d7e8f90", want: "0.1.2+1cdc758"},
		{name: "short sha is kept", version: "0.1.2", commit: "ef8b472", want: "0.1.2+ef8b472"},
		{name: "empty commit falls back to the vcs stamp or stays plain", version: "dev", commit: "", want: "dev"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, commit = tc.version, tc.commit
			got := buildVersion()
			if tc.commit == "" {
				// A build stamped from a git checkout appends its own revision,
				// so only the prefix is deterministic here.
				if got != "dev" && len(got) > len("dev") && got[:4] != "dev+" {
					t.Fatalf("buildVersion() = %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("buildVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildTLSModes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if m, err := buildTLS("off", "", "", "", t.TempDir(), logger); err != nil || m != nil {
		t.Fatalf("off = %+v, %v", m, err)
	}
	if m, err := buildTLS("", "", "", "", t.TempDir(), logger); err != nil || m != nil {
		t.Fatalf("empty mode = %+v, %v", m, err)
	}

	dir := t.TempDir()
	self, err := buildTLS("self-signed", "", "", "mirror.corp.local, 10.2.3.4", dir, logger)
	if err != nil {
		t.Fatalf("self-signed: %v", err)
	}
	if self == nil || self.Mode != tlscert.ModeSelfSigned || !self.Enabled {
		t.Fatalf("self-signed material = %+v", self)
	}
	if len(self.CAPEM) == 0 {
		t.Fatal("自签模式必须导出 CA 供客户端下载")
	}
	joined := strings.Join(self.SANs, ",")
	for _, want := range []string{"mirror.corp.local", "10.2.3.4", "localhost"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sans = %v, 缺少 %s", self.SANs, want)
		}
	}
	for _, path := range []string{self.CertFile, self.KeyFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("缺少证书文件 %s: %v", path, err)
		}
	}

	files, err := buildTLS("files", self.CertFile, self.KeyFile, "", t.TempDir(), logger)
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	if files == nil || files.Mode != tlscert.ModeFiles {
		t.Fatalf("files material = %+v", files)
	}
	if len(files.CAPEM) != 0 {
		t.Fatal("外部证书不应导出 CA")
	}

	if _, err := buildTLS("files", "", "", "", t.TempDir(), logger); err == nil {
		t.Fatal("files 模式缺少证书路径应报错")
	}
	if _, err := buildTLS("files", filepath.Join(dir, "missing.pem"), filepath.Join(dir, "missing.key"), "", t.TempDir(), logger); err == nil {
		t.Fatal("files 模式文件不存在应报错")
	}
	if _, err := buildTLS("bogus", "", "", "", t.TempDir(), logger); err == nil {
		t.Fatal("非法模式应报错")
	}
}
