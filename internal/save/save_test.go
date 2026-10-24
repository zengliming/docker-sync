package save

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"docker-sync/internal/registry"
)

type memSource struct {
	blobs map[string][]byte
}

func (m *memSource) Open(digest string) (io.ReadCloser, error) {
	b, ok := m.blobs[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// plainLayer builds an uncompressed tar layer and returns it together with its
// gzip compressed form.
func plainLayer(t *testing.T, files map[string]string) ([]byte, []byte) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	plain := buf.Bytes()

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return plain, gz.Bytes()
}

func TestBuildAndValidate(t *testing.T) {
	plain, gz := plainLayer(t, map[string]string{"etc/os-release": "NAME=demo\n"})
	layerDigest := digestOf(gz)
	diffID := digestOf(plain)

	cfg := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgDigest := digestOf(cfgBytes)

	src := &memSource{blobs: map[string][]byte{layerDigest: gz, cfgDigest: cfgBytes}}
	img := &registry.Image{
		Ref:            registry.Ref{Registry: "docker.io", Repository: "library/demo", Tag: "1.0"},
		Platform:       "linux/amd64",
		ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		ConfigDigest:   cfgDigest,
		ConfigSize:     int64(len(cfgBytes)),
		Layers:         []registry.Layer{{Digest: layerDigest, Size: int64(len(gz)), MediaType: registry.MediaTypeDockerLayerGzip}},
	}

	out := filepath.Join(t.TempDir(), "demo.tar")
	size, err := Build(context.Background(), out, img, src, Options{RepoTags: []string{"demo:1.0"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if size <= 0 {
		t.Fatalf("Build returned non-positive size %d", size)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() != size {
		t.Fatalf("archive size mismatch: stat=%v size=%d", err, size)
	}
	if err := Validate(out, img); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	entries := readTar(t, out)
	cfgName := strings.TrimPrefix(cfgDigest, "sha256:") + ".json"
	if !bytes.Equal(entries[cfgName], cfgBytes) {
		t.Fatalf("config entry differs from source blob")
	}
	var manifest []manifestEntry
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if len(manifest) != 1 || manifest[0].Config != cfgName {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if len(manifest[0].RepoTags) != 1 || manifest[0].RepoTags[0] != "demo:1.0" {
		t.Fatalf("unexpected repo tags: %+v", manifest[0].RepoTags)
	}
	if len(manifest[0].Layers) != 1 {
		t.Fatalf("unexpected layers: %+v", manifest[0].Layers)
	}
	if got := entries[manifest[0].Layers[0]]; !bytes.Equal(got, plain) {
		t.Fatalf("layer entry is not the uncompressed layer")
	}
}

func TestBuildZstdLayer(t *testing.T) {
	plain, _ := plainLayer(t, map[string]string{"hello.txt": "hi"})
	diffID := digestOf(plain)
	zstdBytes := zstdCompress(t, plain)
	layerDigest := digestOf(zstdBytes)
	cfg := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{diffID}},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgDigest := digestOf(cfgBytes)
	src := &memSource{blobs: map[string][]byte{layerDigest: zstdBytes, cfgDigest: cfgBytes}}
	img := &registry.Image{
		Ref:          registry.Ref{Registry: "docker.io", Repository: "library/demo", Tag: "latest"},
		Platform:     "linux/amd64",
		ConfigDigest: cfgDigest,
		Layers:       []registry.Layer{{Digest: layerDigest, Size: int64(len(zstdBytes)), MediaType: registry.MediaTypeOCILayerZstd}},
	}
	out := filepath.Join(t.TempDir(), "zstd.tar")
	if _, err := Build(context.Background(), out, img, src, Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Validate(out, img); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsCorruptArchive(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bad.tar")
	if err := os.WriteFile(out, []byte("not a tar archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(out, nil); err == nil {
		t.Fatal("Validate accepted a corrupt archive")
	}
}

func TestBuildFailsOnDiffIDMismatch(t *testing.T) {
	_, gz := plainLayer(t, map[string]string{"a": "b"})
	layerDigest := digestOf(gz)
	cfg := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + strings.Repeat("0", 64)}},
	}
	cfgBytes, _ := json.Marshal(cfg)
	cfgDigest := digestOf(cfgBytes)
	src := &memSource{blobs: map[string][]byte{layerDigest: gz, cfgDigest: cfgBytes}}
	img := &registry.Image{
		Ref:          registry.Ref{Registry: "docker.io", Repository: "library/demo", Tag: "latest"},
		Platform:     "linux/amd64",
		ConfigDigest: cfgDigest,
		Layers:       []registry.Layer{{Digest: layerDigest}},
	}
	if _, err := Build(context.Background(), filepath.Join(t.TempDir(), "bad.tar"), img, src, Options{}); err == nil {
		t.Fatal("Build accepted a layer whose diff id does not match the config")
	}
}

func zstdCompress(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readTar(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = b
	}
	return out
}
