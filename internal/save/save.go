// Package save writes a docker-save compatible tar archive ("docker load"
// format) from an image that was pulled directly from a registry.
package save

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"docker-sync/internal/registry"
)

// Source provides the raw blobs (config + layers) referenced by an Image.
type Source interface {
	Open(digest string) (io.ReadCloser, error)
}

// Options controls archive naming metadata.
type Options struct {
	// RepoTags is written verbatim to manifest.json, e.g. ["alpine:3.20"].
	RepoTags []string
	// Platform is informational (linux/amd64); stored in the config file if
	// the upstream config does not carry it.
	Platform string
	// Created is the archive timestamp for audit purposes.
	Created time.Time
}

type manifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

type layerFile struct {
	path   string
	size   int64
	diffID string
}

// Build writes a docker load compatible tar archive to archivePath and returns
// its size. Layers are decompressed into plain tar entries (the canonical
// docker save format) so that "docker load -i <archive>" works everywhere.
func Build(ctx context.Context, archivePath string, img *registry.Image, src Source, opts Options) (int64, error) {
	if img == nil {
		return 0, errors.New("save: nil image")
	}
	if src == nil {
		return 0, errors.New("save: nil source")
	}
	if strings.TrimSpace(archivePath) == "" {
		return 0, errors.New("save: empty archive path")
	}
	created := opts.Created
	if created.IsZero() {
		created = time.Now()
	}
	dir := filepath.Dir(archivePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("save: create archive directory: %w", err)
	}

	// The config blob is small and is embedded verbatim in the archive.
	cfgRC, err := src.Open(img.ConfigDigest)
	if err != nil {
		return 0, fmt.Errorf("save: open image config %s: %w", img.ConfigDigest, err)
	}
	cfgBytes, err := io.ReadAll(io.LimitReader(cfgRC, 64<<20))
	_ = cfgRC.Close()
	if err != nil {
		return 0, fmt.Errorf("save: read image config %s: %w", img.ConfigDigest, err)
	}
	cfgID := strings.TrimPrefix(img.ConfigDigest, "sha256:")
	if sum := sha256.Sum256(cfgBytes); hex.EncodeToString(sum[:]) != cfgID {
		return 0, fmt.Errorf("save: image config %s content does not match its digest", img.ConfigDigest)
	}
	var cfg imageConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return 0, fmt.Errorf("save: decode image config %s: %w", img.ConfigDigest, err)
	}
	if len(cfg.RootFS.DiffIDs) != 0 && len(cfg.RootFS.DiffIDs) != len(img.Layers) {
		return 0, fmt.Errorf("save: image config lists %d diff ids but manifest has %d layers", len(cfg.RootFS.DiffIDs), len(img.Layers))
	}

	tmpDir, err := os.MkdirTemp(dir, ".docker-save-")
	if err != nil {
		return 0, fmt.Errorf("save: create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	layers := make([]layerFile, len(img.Layers))
	for i, l := range img.Layers {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rc, err := src.Open(l.Digest)
		if err != nil {
			return 0, fmt.Errorf("save: open layer %s: %w", l.Digest, err)
		}
		path := filepath.Join(tmpDir, fmt.Sprintf("layer-%d.tar", i))
		diffID, size, derr := decodeLayer(ctx, rc, path)
		_ = rc.Close()
		if derr != nil {
			return 0, fmt.Errorf("save: layer %s: %w", l.Digest, derr)
		}
		layers[i] = layerFile{path: path, size: size, diffID: diffID}
		if i < len(cfg.RootFS.DiffIDs) && cfg.RootFS.DiffIDs[i] != "" && cfg.RootFS.DiffIDs[i] != diffID {
			return 0, fmt.Errorf("save: layer %s diff id is %s, image config expects %s", l.Digest, diffID, cfg.RootFS.DiffIDs[i])
		}
	}

	names := layerNames(img)
	entry := manifestEntry{
		Config:   cfgID + ".json",
		RepoTags: repoTags(img, opts),
		Layers:   names,
	}
	manifestBytes, err := json.Marshal([]manifestEntry{entry})
	if err != nil {
		return 0, fmt.Errorf("save: encode manifest.json: %w", err)
	}

	tmpPath := archivePath + ".tmp"
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("save: create archive: %w", err)
	}
	tw := tar.NewWriter(f)
	if err := writeBytes(tw, "manifest.json", manifestBytes, created); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("save: write manifest.json: %w", err)
	}
	if err := writeBytes(tw, entry.Config, cfgBytes, created); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("save: write image config: %w", err)
	}
	for i, lf := range layers {
		hdr := &tar.Header{
			Name:     names[i],
			Mode:     0o644,
			Size:     lf.size,
			ModTime:  created,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("save: write layer header %s: %w", names[i], err)
		}
		if err := copyFile(ctx, tw, lf.path, lf.size); err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("save: write layer %s: %w", names[i], err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("save: finish archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("save: close archive: %w", err)
	}
	st, err := os.Stat(tmpPath)
	if err != nil {
		return 0, fmt.Errorf("save: stat archive: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		// Windows may refuse to replace an existing file in some situations.
		if rmErr := os.Remove(archivePath); rmErr != nil && !os.IsNotExist(rmErr) {
			return 0, fmt.Errorf("save: replace archive: %w", err)
		}
		if err := os.Rename(tmpPath, archivePath); err != nil {
			return 0, fmt.Errorf("save: finalize archive: %w", err)
		}
	}
	success = true
	return st.Size(), nil
}

// Validate re-reads a generated archive and checks that it is a well formed
// docker save archive whose config and layer digests match the image. It is
// used by the service before publishing a tar and by the test suite.
func Validate(archivePath string, img *registry.Image) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("save: open archive: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	entrySize := map[string]int64{}
	var manifestBytes, configBytes []byte
	var configName string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("save: read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.ToSlash(hdr.Name)
		entrySize[name] = hdr.Size
		switch {
		case name == "manifest.json":
			manifestBytes, err = io.ReadAll(io.LimitReader(tr, 8<<20))
			if err != nil {
				return fmt.Errorf("save: read manifest.json: %w", err)
			}
		case strings.HasSuffix(name, ".json"):
			// The image config is the only other json file we write.
			configName = name
			configBytes, err = io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return fmt.Errorf("save: read image config: %w", err)
			}
		}
	}
	if len(manifestBytes) == 0 {
		return errors.New("save: archive has no manifest.json")
	}
	var entries []manifestEntry
	if err := json.Unmarshal(manifestBytes, &entries); err != nil {
		return fmt.Errorf("save: manifest.json is invalid: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("save: manifest.json is empty")
	}
	if configBytes == nil {
		return errors.New("save: archive has no image config json")
	}
	var cfg imageConfig
	if err := json.Unmarshal(configBytes, &cfg); err != nil {
		return fmt.Errorf("save: image config is invalid json: %w", err)
	}
	for _, e := range entries {
		if e.Config == "" {
			return errors.New("save: manifest entry has no Config")
		}
		if _, ok := entrySize[e.Config]; !ok {
			return fmt.Errorf("save: manifest references missing config %q", e.Config)
		}
		if configName != "" && e.Config != configName {
			return fmt.Errorf("save: manifest config %q does not match archive config %q", e.Config, configName)
		}
		if len(e.RepoTags) == 0 {
			return errors.New("save: manifest entry has no RepoTags")
		}
		if len(cfg.RootFS.DiffIDs) != 0 && len(cfg.RootFS.DiffIDs) != len(e.Layers) {
			return fmt.Errorf("save: config lists %d diff ids but manifest has %d layers", len(cfg.RootFS.DiffIDs), len(e.Layers))
		}
		for _, l := range e.Layers {
			if _, ok := entrySize[l]; !ok {
				return fmt.Errorf("save: manifest references missing layer %q", l)
			}
		}
	}
	if img != nil {
		wantConfig := strings.TrimPrefix(img.ConfigDigest, "sha256:") + ".json"
		wantLayers := layerNames(img)
		for _, e := range entries {
			if e.Config != wantConfig {
				return fmt.Errorf("save: archive config %q, expected %q", e.Config, wantConfig)
			}
			if len(e.Layers) != len(wantLayers) {
				return fmt.Errorf("save: archive has %d layers, expected %d", len(e.Layers), len(wantLayers))
			}
			for i := range wantLayers {
				if e.Layers[i] != wantLayers[i] {
					return fmt.Errorf("save: archive layer %d is %q, expected %q", i, e.Layers[i], wantLayers[i])
				}
			}
		}
	}
	return nil
}

func repoTags(img *registry.Image, opts Options) []string {
	if len(opts.RepoTags) > 0 {
		return opts.RepoTags
	}
	if ref := img.Ref.String(); ref != "" {
		return []string{ref}
	}
	return nil
}

// layerNames returns the in-archive paths of the layers, matching the classic
// "<digest hex>/layer.tar" layout produced by docker save.
func layerNames(img *registry.Image) []string {
	names := make([]string, len(img.Layers))
	for i, l := range img.Layers {
		names[i] = strings.TrimPrefix(l.Digest, "sha256:") + "/layer.tar"
	}
	return names
}

func writeBytes(tw *tar.Writer, name string, data []byte, mod time.Time) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		ModTime:  mod,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func copyFile(ctx context.Context, dst io.Writer, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := copyCtx(ctx, dst, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("short read: expected %d bytes, got %d", size, n)
	}
	return nil
}

// decodeLayer decompresses one layer blob to dst and returns its sha256
// "diff id" and uncompressed size. Compression is detected from the content
// itself (gzip/zstd magic) rather than from the media type, which some
// registries report inaccurately.
func decodeLayer(ctx context.Context, rc io.Reader, dst string) (string, int64, error) {
	br := bufio.NewReaderSize(rc, 64<<10)
	magic, _ := br.Peek(4)
	var r io.Reader
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		zr, err := gzip.NewReader(br)
		if err != nil {
			return "", 0, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		r = zr
	case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return "", 0, fmt.Errorf("zstd: %w", err)
		}
		defer zr.Close()
		r = zr
	default:
		r = br
	}

	f, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := copyCtx(ctx, io.MultiWriter(f, h), r)
	cerr := f.Close()
	if err != nil {
		return "", 0, err
	}
	if cerr != nil {
		return "", 0, cerr
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return n, werr
			}
			n += int64(m)
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}
