// Command imagebuild assembles a runnable docker-sync image tar without a docker
// daemon: it pulls the base image through the Registry v2 API, appends an
// application layer holding the static binary, and writes the result in
// `docker save` format so the archive can be loaded with `docker load -i`.
//
// It is a maintainer helper, not part of the shipped service.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-sync/internal/cas"
	"docker-sync/internal/registry"
	"docker-sync/internal/save"
)

const (
	appUser  = 10001
	appGroup = 10001

	caBundlePath = "etc/ssl/certs/ca-certificates.crt"
)

// casSource adapts the content addressed store to the saver's Source interface.
type casSource struct{ store *cas.Store }

func (s casSource) Open(digest string) (io.ReadCloser, error) { return s.store.Open(digest) }

// layerFile describes one file placed into the application layer.
type layerFile struct {
	name    string
	mode    int64
	uid     int
	gid     int
	dir     bool
	content []byte
}

func main() {
	base := flag.String("base", "alpine:3.22", "base image reference")
	platform := flag.String("platform", "linux/amd64", "target platform")
	binary := flag.String("binary", "bin/docker-sync-linux-amd64", "statically linked linux binary")
	certs := flag.String("certs", "", "PEM CA bundle, only used when the base image ships none")
	entrypoint := flag.String("entrypoint", "", "optional entrypoint script copied to /entrypoint.sh; leave empty to exec /app/docker-sync directly")
	tag := flag.String("tag", "docker-sync:0.1.4", "primary repo tag")
	extraTag := flag.String("extra-tag", "docker-sync:latest", "additional repo tag (empty to skip)")
	version := flag.String("version", "0.1.4", "image version label")
	commit := flag.String("commit", "", "git revision label")
	out := flag.String("out", "dist/docker-sync-image-linux-amd64.tar", "output archive")
	flag.Parse()

	if err := run(*base, *platform, *binary, *certs, *entrypoint, *tag, *extraTag, *version, *commit, *out); err != nil {
		fmt.Fprintln(os.Stderr, "imagebuild:", err)
		os.Exit(1)
	}
}

func run(base, platform, binaryPath, certsPath, entrypointPath, tag, extraTag, version, commit, outPath string) error {
	ctx := context.Background()

	work, err := os.MkdirTemp("", "imagebuild-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	blobs, err := cas.New(filepath.Join(work, "blobs"))
	if err != nil {
		return err
	}

	client := registry.New(registry.Options{UserAgent: "docker-sync-imagebuild"})
	ref, err := registry.Parse(base)
	if err != nil {
		return err
	}
	img, err := client.Resolve(ctx, ref, platform)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", base, err)
	}
	stats, err := client.Download(ctx, img, blobs, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", base, err)
	}
	fmt.Printf("base %s: %d layers, %d bytes fetched (%d reused)\n",
		base, len(img.Layers), stats.BytesDownloaded, stats.BlobsReused)

	baseConfig, err := readConfig(blobs, img.ConfigDigest)
	if err != nil {
		return err
	}

	baseFiles, err := readBaseFiles(blobs, img.Layers, []string{"etc/passwd", "etc/group", caBundlePath})
	if err != nil {
		return err
	}
	if len(baseFiles[caBundlePath]) > 0 {
		fmt.Printf("base image already ships a CA bundle, keeping it\n")
	} else if certsPath == "" {
		fmt.Printf("warning: base image has no CA bundle and -certs is empty; HTTPS upstreams may fail\n")
	}

	appLayer, appDiffID, hasEntrypoint, err := buildAppLayer(binaryPath, certsPath, entrypointPath, baseFiles)
	if err != nil {
		return err
	}
	appDigest, appSize, err := storeLayer(ctx, blobs, appLayer)
	if err != nil {
		return err
	}
	fmt.Printf("app layer %s (%d bytes gzip, diff %s)\n", appDigest, appSize, appDiffID)

	entryCmd := []string{"/app/docker-sync"}
	if hasEntrypoint {
		entryCmd = []string{"/entrypoint.sh"}
		fmt.Printf("entrypoint /entrypoint.sh\n")
	}
	// 不设置 User：程序以 root 启动，自己 chown 数据目录后 setuid 降权，
	// 不依赖镜像里是否存在 su-exec/gosu 之类的工具。
	configJSON, err := buildConfig(baseConfig, appDiffID, version, commit, entryCmd, "")
	if err != nil {
		return err
	}
	cfgSum := sha256.Sum256(configJSON)
	cfgDigest := "sha256:" + hex.EncodeToString(cfgSum[:])
	if _, err := blobs.PutBytes(ctx, cfgDigest, configJSON); err != nil {
		return err
	}

	built := &registry.Image{
		Ref:            ref,
		Platform:       platform,
		MediaType:      registry.MediaTypeDockerManifest,
		ConfigDigest:   cfgDigest,
		ConfigSize:     int64(len(configJSON)),
		ManifestDigest: img.ManifestDigest,
		Layers: append(append([]registry.Layer{}, img.Layers...), registry.Layer{
			Digest:    appDigest,
			Size:      appSize,
			MediaType: registry.MediaTypeDockerLayerGzip,
		}),
	}

	tags := []string{tag}
	if extraTag != "" && extraTag != tag {
		tags = append(tags, extraTag)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	size, err := save.Build(ctx, outPath, built, casSource{store: blobs}, save.Options{
		RepoTags: tags,
		Platform: platform,
		Created:  time.Now(),
	})
	if err != nil {
		return fmt.Errorf("build archive: %w", err)
	}
	if err := save.Validate(outPath, built); err != nil {
		return fmt.Errorf("validate archive: %w", err)
	}

	sum, err := fileSHA256(outPath)
	if err != nil {
		return err
	}
	fmt.Printf("archive %s\n  size   %d bytes\n  sha256 %s\n  tags   %v\n", outPath, size, sum, tags)
	return nil
}

// baseConfig is the part of an image config the builder needs to reuse.
type baseConfig struct {
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
	History []json.RawMessage `json:"history"`
}

func readConfig(blobs *cas.Store, digest string) (baseConfig, error) {
	var cfg baseConfig
	rc, err := blobs.Open(digest)
	if err != nil {
		return cfg, err
	}
	defer rc.Close()
	if err := json.NewDecoder(rc).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode base config: %w", err)
	}
	return cfg, nil
}

// readBaseFiles extracts the requested files from the base image layers. Later
// layers win, mirroring how an overlay stack resolves paths.
func readBaseFiles(blobs *cas.Store, layers []registry.Layer, names []string) (map[string][]byte, error) {
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[n] = true
	}
	found := make(map[string][]byte, len(names))
	for _, layer := range layers {
		rc, err := blobs.Open(layer.Digest)
		if err != nil {
			return nil, err
		}
		content, err := scanTarFor(rc, wanted)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("scan layer %s: %w", layer.Digest, err)
		}
		for name, data := range content {
			found[name] = data
		}
	}
	return found, nil
}

// scanTarFor decompresses a layer if needed and collects the wanted files.
func scanTarFor(r io.Reader, wanted map[string]bool) (map[string][]byte, error) {
	br := bufio.NewReader(r)
	head, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, err
	}
	var plain io.Reader = br
	if len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		plain = zr
	}

	found := make(map[string][]byte)
	tr := tar.NewReader(plain)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if !wanted[name] || hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		found[name] = data
	}
	return found, nil
}

// buildAppLayer renders the application layer and returns it compressed along
// with the sha256 of the plain (uncompressed) tar, which is the diff id.
//
// baseFiles holds files inherited from the base image (passwd, group, CA
// bundle). They are extended or left alone instead of being overwritten, so the
// image keeps the distro's own users and trust store.
func buildAppLayer(binaryPath, certsPath, entrypointPath string, baseFiles map[string][]byte) ([]byte, string, bool, error) {
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return nil, "", false, err
	}

	created := time.Unix(1700000000, 0).UTC()
	files := []layerFile{
		{name: "app", mode: 0o755, uid: appUser, gid: appGroup, dir: true},
		{name: "app/docker-sync", mode: 0o755, uid: appUser, gid: appGroup, content: binary},
		{name: "data", mode: 0o755, uid: appUser, gid: appGroup, dir: true},
		{name: "etc/passwd", mode: 0o644, content: appendPasswd(baseFiles["etc/passwd"])},
		{name: "etc/group", mode: 0o644, content: appendGroup(baseFiles["etc/group"])},
	}

	// 入口脚本归属 root，因为它需要在启动时 chown 挂载目录，之后自行降权。
	hasEntrypoint := false
	if entrypointPath != "" {
		script, err := os.ReadFile(entrypointPath)
		if err != nil {
			return nil, "", false, fmt.Errorf("read entrypoint: %w", err)
		}
		files = append(files, layerFile{name: "entrypoint.sh", mode: 0o755, uid: 0, gid: 0, content: script})
		hasEntrypoint = true
	}

	// Only ship a CA bundle when the base image has none: distro images already
	// carry a maintained one and overwriting it would drop their updates.
	if len(baseFiles[caBundlePath]) == 0 && certsPath != "" {
		certs, err := os.ReadFile(certsPath)
		if err != nil {
			return nil, "", false, err
		}
		files = append(files,
			layerFile{name: "etc/ssl", mode: 0o755, dir: true},
			layerFile{name: "etc/ssl/certs", mode: 0o755, dir: true},
			layerFile{name: caBundlePath, mode: 0o644, content: certs},
		)
	}

	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	for _, f := range files {
		hdr := &tar.Header{
			Name:    f.name,
			Mode:    f.mode,
			Uid:     f.uid,
			Gid:     f.gid,
			ModTime: created,
			Format:  tar.FormatPAX,
		}
		if f.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Name = f.name + "/"
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", false, err
		}
		if !f.dir {
			if _, err := tw.Write(f.content); err != nil {
				return nil, "", false, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", false, err
	}

	sum := sha256.Sum256(plain.Bytes())
	diffID := "sha256:" + hex.EncodeToString(sum[:])

	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return nil, "", false, err
	}
	zw.Header.ModTime = created
	if _, err := zw.Write(plain.Bytes()); err != nil {
		return nil, "", false, err
	}
	if err := zw.Close(); err != nil {
		return nil, "", false, err
	}
	return gz.Bytes(), diffID, hasEntrypoint, nil
}

// storeLayer writes the compressed layer into the CAS and returns its digest.
func storeLayer(ctx context.Context, blobs *cas.Store, layer []byte) (string, int64, error) {
	sum := sha256.Sum256(layer)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	size, err := blobs.PutBytes(ctx, digest, layer)
	if err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

// buildConfig renders the image config: base diff ids plus the app layer, and a
// history entry so the layer count matches.
func buildConfig(base baseConfig, appDiffID, version, commit string, entryCmd []string, runUser string) ([]byte, error) {
	created := time.Now().UTC().Format(time.RFC3339)
	diffIDs := append(append([]string{}, base.RootFS.DiffIDs...), appDiffID)
	history := append([]json.RawMessage{}, base.History...)
	createdBy := "COPY /out/docker-sync /app/docker-sync # docker-sync " + version
	if commit != "" {
		createdBy += "+" + commit
	}
	entry, err := json.Marshal(map[string]any{
		"created":    created,
		"created_by": createdBy,
		"comment":    "docker-sync " + version,
	})
	if err != nil {
		return nil, err
	}
	history = append(history, entry)

	runtime := map[string]any{
		"Env": []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		"Entrypoint":   entryCmd,
		"Cmd":          []string{"-addr", ":8080", "-data", "/data"},
		"WorkingDir":   "/app",
		"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
		"Volumes":      map[string]any{"/data": map[string]any{}},
		"StopSignal":   "SIGTERM",
		"Labels":       imageLabels(version, commit),
		"Healthcheck": map[string]any{
			"Test": []string{
				"CMD-SHELL",
				"wget -q -O /dev/null http://127.0.0.1:8080/api/health || wget -q --no-check-certificate -O /dev/null https://127.0.0.1:8080/api/health || exit 1",
			},
			"Interval":    int64(30 * time.Second),
			"Timeout":     int64(5 * time.Second),
			"StartPeriod": int64(10 * time.Second),
			"Retries":     3,
		},
		"AttachStdin":  false,
		"AttachStdout": false,
		"AttachStderr": false,
		"Tty":          false,
		"OpenStdin":    false,
		"StdinOnce":    false,
	}
	// runUser 为空表示以 root 启动，由入口脚本修正挂载目录后自行降权。
	if runUser != "" {
		runtime["User"] = runUser
	}

	cfg := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      created,
		"config":       runtime,
		"rootfs":       map[string]any{"type": "layers", "diff_ids": diffIDs},
		"history":      history,
	}
	return json.Marshal(cfg)
}

func imageLabels(version, commit string) map[string]string {
	labels := map[string]string{
		"org.opencontainers.image.title":       "docker-sync",
		"org.opencontainers.image.description": "Docker 镜像同步与镜像加速服务",
		"org.opencontainers.image.version":     version,
		"org.opencontainers.image.source":      "https://github.com/zengliming/docker-sync",
		"org.opencontainers.image.licenses":    "MIT",
	}
	if commit != "" {
		labels["org.opencontainers.image.revision"] = commit
	}
	return labels
}

// appendPasswd adds the service account, keeping everything the base image had.
func appendPasswd(base []byte) []byte {
	const entry = "docker-sync:x:10001:10001:docker-sync:/app:/sbin/nologin"
	if bytes.Contains(base, []byte("docker-sync:")) {
		return base
	}
	out := append([]byte{}, base...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, entry+"\n"...)
}

// appendGroup mirrors appendPasswd for /etc/group.
func appendGroup(base []byte) []byte {
	const entry = "docker-sync:x:10001:"
	if bytes.Contains(base, []byte("docker-sync:")) {
		return base
	}
	out := append([]byte{}, base...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, entry+"\n"...)
}

func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
