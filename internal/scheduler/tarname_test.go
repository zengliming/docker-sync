package scheduler

import (
	"strings"
	"testing"

	"docker-sync/internal/registry"
)

func TestTarNameMatchesContract(t *testing.T) {
	ref := registry.Ref{
		Registry:   "docker.io",
		Repository: "library/alpine",
		Tag:        "3.20",
	}
	got := TarName(ref, "linux/amd64", "sha256:1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809")
	want := "library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar"
	if got != want {
		t.Fatalf("TarName() = %q, want %q", got, want)
	}
}

func TestTarNameDigestPinnedReference(t *testing.T) {
	ref := registry.Ref{
		Registry:   "ghcr.io",
		Repository: "owner/app",
		Digest:     "sha256:cadef01234567890",
	}
	got := TarName(ref, "linux/arm64/v8", "sha256:abcdef123456")
	want := "owner_app_linux_arm64_v8_abcdef123456.tar"
	if got != want {
		t.Fatalf("TarName() = %q, want %q", got, want)
	}
}

func TestTarNameSanitizesUnsafeParts(t *testing.T) {
	ref := registry.Ref{Repository: "we ird/repo", Tag: "v1+build"}
	got := TarName(ref, "linux/amd64", "sha256:0123456789abcdef")
	if !isValidTarName(got) {
		t.Fatalf("TarName() = %q, not a valid tar name", got)
	}
	if !strings.HasSuffix(got, ".tar") {
		t.Fatalf("TarName() = %q, want .tar suffix", got)
	}
	if strings.ContainsAny(got, " /+") {
		t.Fatalf("TarName() = %q, contains unsafe characters", got)
	}
}

func isValidTarName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func TestValidateCron(t *testing.T) {
	for _, ok := range []string{"* * * * *", "0 */2 * * *", "15 3 * * 1-5", "0 0 1 * *"} {
		if err := ValidateCron(ok); err != nil {
			t.Fatalf("ValidateCron(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "* * * *", "* * * * * *", "not a cron"} {
		if err := ValidateCron(bad); err == nil {
			t.Fatalf("ValidateCron(%q) = nil, want error", bad)
		}
	}
}
