package save_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"docker-sync/internal/cas"
	"docker-sync/internal/registry"
	"docker-sync/internal/save"
)

// TestLivePullAlpine exercises the full pipeline against Docker Hub: resolve a
// manifest, download config and layers into the CAS, then build and validate a
// docker save archive. It only runs when DOCKER_SYNC_E2E=1.
func TestLivePullAlpine(t *testing.T) {
	if os.Getenv("DOCKER_SYNC_E2E") == "" {
		t.Skip("set DOCKER_SYNC_E2E=1 to run the live registry test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref, err := registry.Parse("alpine:3.20")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	client := registry.New(registry.Options{MaxConcurrentDownloads: 4})
	img, err := client.Resolve(ctx, ref, "linux/amd64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(img.Layers) == 0 {
		t.Fatal("Resolve returned no layers")
	}
	t.Logf("resolved %s platform=%s manifest=%s layers=%d config=%s", img.Ref, img.Platform, img.ManifestDigest, len(img.Layers), img.ConfigDigest)

	store, err := cas.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	stats, err := client.Download(ctx, img, store, func(done, total int, l registry.Layer) {
		t.Logf("blob %d/%d %s (%d bytes)", done, total, l.Digest, l.Size)
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	t.Logf("downloaded %d blobs (%d bytes), reused %d", stats.BlobsDownloaded, stats.BytesDownloaded, stats.BlobsReused)
	if stats.BlobsDownloaded != len(img.Layers)+1 {
		t.Fatalf("expected %d downloaded blobs, got %d", len(img.Layers)+1, stats.BlobsDownloaded)
	}

	tarPath := filepath.Join(t.TempDir(), "alpine-3.20-linux-amd64.tar")
	size, err := save.Build(ctx, tarPath, img, store, save.Options{RepoTags: []string{"alpine:3.20"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := save.Validate(tarPath, img); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	t.Logf("built %s (%d bytes)", tarPath, size)

	// A second download must hit the CAS and transfer nothing.
	again, err := client.Download(ctx, img, store, nil)
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	if again.BlobsDownloaded != 0 || again.BlobsReused != len(img.Layers)+1 {
		t.Fatalf("cache not reused: downloaded=%d reused=%d", again.BlobsDownloaded, again.BlobsReused)
	}
}
