// Package mirror implements a read-only pull-through cache that speaks the
// Docker Registry v2 protocol. Docker daemons can be pointed at it directly
// ("docker pull <host:port>/library/alpine:3.20") or through
// "registry-mirrors" and every pull is then served from the local blob store
// once it has been cached.
//
// It deliberately does not build archives: it only proxies manifests and blobs
// and shares the store's content addressable blob cache with the sync jobs.
package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"docker-sync/internal/cas"
	"docker-sync/internal/model"
	"docker-sync/internal/registry"
)

// manifestLimit caps how much of a manifest document is buffered in memory.
const manifestLimit = 8 << 20

// Upstream fetches documents from one upstream registry host.
type Upstream interface {
	// RawManifest returns the manifest document without following an index.
	RawManifest(ctx context.Context, ref registry.Ref, identifier string) (registry.RawManifest, error)
	// OpenBlob starts streaming a blob.
	OpenBlob(ctx context.Context, ref registry.Ref, digest string) (io.ReadCloser, int64, error)
	// StatBlob reports the size of a blob without transferring it.
	StatBlob(ctx context.Context, ref registry.Ref, digest string) (int64, error)
}

// Upstreams resolves a registry host name to a client.
type Upstreams interface {
	Upstream(host string) (Upstream, error)
}

// Stats counts how effective the cache is.
type Stats struct {
	ManifestHits   int64 `json:"manifestHits"`
	ManifestMisses int64 `json:"manifestMisses"`
	BlobHits       int64 `json:"blobHits"`
	BlobMisses     int64 `json:"blobMisses"`
	BytesServed    int64 `json:"bytesServed"`
}

// Options configures New.
type Options struct {
	// Settings returns the current settings snapshot.
	Settings func() model.Settings
	// Blobs is the shared blob cache.
	Blobs *cas.Store
	// Upstreams resolves upstream hosts.
	Upstreams Upstreams
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Server serves the Registry v2 read endpoints backed by a blob cache.
type Server struct {
	settings  func() model.Settings
	blobs     *cas.Store
	upstreams Upstreams
	log       *slog.Logger

	mu    sync.Mutex
	tags  map[string]tagEntry
	stats Stats
}

type tagEntry struct {
	digest    string
	mediaType string
	fetched   time.Time
}

// New validates opts and returns a mirror server.
func New(opts Options) (*Server, error) {
	if opts.Settings == nil {
		return nil, errors.New("mirror: settings func is required")
	}
	if opts.Blobs == nil {
		return nil, errors.New("mirror: blob store is required")
	}
	if opts.Upstreams == nil {
		return nil, errors.New("mirror: upstream resolver is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		settings:  opts.Settings,
		blobs:     opts.Blobs,
		upstreams: opts.Upstreams,
		log:       logger,
		tags:      make(map[string]tagEntry),
	}, nil
}

// Stats returns a snapshot of the cache counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Invalidate drops the cached tag -> digest mappings so the next pull asks the
// upstream again. It is called when the settings change.
func (s *Server) Invalidate() {
	s.mu.Lock()
	s.tags = make(map[string]tagEntry)
	s.mu.Unlock()
}

// Handler returns the /v2/ endpoint handler.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")

	cfg := s.settings().Mirror
	rest := strings.TrimPrefix(r.URL.Path, "/v2")
	rest = strings.TrimPrefix(rest, "/")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED",
			"镜像加速站点是只读缓存，只支持 GET 和 HEAD")
		return
	}
	if rest == "" {
		if !cfg.Enabled {
			writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "镜像加速未启用")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
		return
	}
	if !cfg.Enabled {
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "镜像加速未启用，请在设置页打开")
		return
	}
	if strings.Contains(rest, "..") {
		writeRegistryError(w, http.StatusBadRequest, "NAME_INVALID", "镜像名称不合法")
		return
	}

	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		s.serveManifest(w, r, cfg, rest[:i], rest[i+len("/manifests/"):])
		return
	}
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		s.serveBlob(w, r, cfg, rest[:i], rest[i+len("/blobs/"):])
		return
	}
	writeRegistryError(w, http.StatusNotFound, "UNSUPPORTED",
		"镜像加速站点只实现 manifest 与 blob 的读取接口")
}

// route resolves the upstream host and repository for a request name.
func (s *Server) route(cfg model.MirrorSettings, name string) (string, string, error) {
	first := name
	rest := ""
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first, rest = name[:i], name[i+1:]
	}

	host := ""
	for _, up := range cfg.Upstreams {
		if strings.EqualFold(first, up) {
			host = up
			break
		}
	}
	if host != "" {
		name = rest
	} else if cfg.AllowAll && rest != "" && strings.ContainsAny(first, ".:") {
		host = strings.ToLower(first)
		name = rest
	} else {
		host = cfg.Upstreams[0]
	}

	if host == "" {
		host = defaultUpstream
	}
	if !validRepo(name) {
		return "", "", fmt.Errorf("镜像仓库名不合法: %q", name)
	}
	return host, name, nil
}

// defaultUpstream is the fallback used when settings carry no upstream at all.
const defaultUpstream = "docker.io"

func validRepo(repo string) bool {
	if repo == "" || len(repo) > 255 {
		return false
	}
	if strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}
	return true
}

// isDigest reports whether s is a sha256 content digest.
func isDigest(s string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) || len(s) != len(prefix)+64 {
		return false
	}
	for _, r := range s[len(prefix):] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// validTag reports whether s looks like a tag.
func validTag(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Server) serveManifest(w http.ResponseWriter, r *http.Request, cfg model.MirrorSettings, name, identifier string) {
	host, repo, err := s.route(cfg, name)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "NAME_INVALID", err.Error())
		return
	}
	up, err := s.upstreams.Upstream(host)
	if err != nil {
		writeRegistryError(w, http.StatusBadGateway, "UNKNOWN", err.Error())
		return
	}
	ref := registry.Ref{Registry: host, Repository: repo}
	ctx := r.Context()

	if isDigest(identifier) {
		if body, ok := s.readCached(identifier); ok {
			s.countManifest(true)
			serveManifestBytes(w, r, identifier, mediaTypeOf(body), body)
			return
		}
		rm, err := up.RawManifest(ctx, ref, identifier)
		if err != nil {
			s.writeUpstreamError(w, "MANIFEST_UNKNOWN", "拉取 manifest 失败", err)
			return
		}
		s.countManifest(false)
		if rm.Digest != identifier {
			writeRegistryError(w, http.StatusBadGateway, "UNKNOWN",
				fmt.Sprintf("上游返回的 manifest 摘要 %s 与请求的 %s 不一致", rm.Digest, identifier))
			return
		}
		s.cacheManifest(ctx, rm)
		serveManifestBytes(w, r, rm.Digest, rm.MediaType, rm.Body)
		return
	}

	if !validTag(identifier) {
		writeRegistryError(w, http.StatusBadRequest, "MANIFEST_INVALID",
			fmt.Sprintf("manifest 引用不合法: %q", identifier))
		return
	}

	key := host + "/" + repo + "@" + identifier
	if entry, ok := s.lookupTag(key, cfg.TagTTLMinutes); ok {
		if body, ok := s.readCached(entry.digest); ok {
			s.countManifest(true)
			serveManifestBytes(w, r, entry.digest, mediaTypeOf(body), body)
			return
		}
	}

	rm, err := up.RawManifest(ctx, ref, identifier)
	if err != nil {
		s.writeUpstreamError(w, "MANIFEST_UNKNOWN", "拉取 manifest 失败", err)
		return
	}
	s.countManifest(false)
	s.cacheManifest(ctx, rm)
	s.storeTag(key, rm.Digest, rm.MediaType)
	serveManifestBytes(w, r, rm.Digest, rm.MediaType, rm.Body)
}

// cacheManifest writes a manifest document into the blob cache; failures are
// logged but never break the response.
func (s *Server) cacheManifest(ctx context.Context, rm registry.RawManifest) {
	if !isDigest(rm.Digest) {
		return
	}
	if _, err := s.blobs.PutBytes(ctx, rm.Digest, rm.Body); err != nil {
		s.log.Warn("镜像加速：缓存 manifest 失败", "digest", rm.Digest, "error", err)
	}
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, cfg model.MirrorSettings, name, digest string) {
	if strings.HasPrefix(digest, "uploads") {
		writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED",
			"镜像加速站点不支持上传，请使用 docker-sync 同步任务")
		return
	}
	if !isDigest(digest) {
		writeRegistryError(w, http.StatusBadRequest, "DIGEST_INVALID",
			fmt.Sprintf("blob 摘要不合法: %q", digest))
		return
	}
	host, repo, err := s.route(cfg, name)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, "NAME_INVALID", err.Error())
		return
	}
	up, err := s.upstreams.Upstream(host)
	if err != nil {
		writeRegistryError(w, http.StatusBadGateway, "UNKNOWN", err.Error())
		return
	}
	ref := registry.Ref{Registry: host, Repository: repo}
	ctx := r.Context()

	if f, size, err := s.blobs.OpenFile(digest); err == nil {
		defer f.Close()
		s.countBlob(true)
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Docker-Content-Digest", digest)
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		cw := &countingWriter{w: w}
		http.ServeContent(cw, r, "", time.Time{}, f)
		s.addBytes(cw.n)
		return
	}

	// Cold cache. A HEAD only needs the size, a ranged GET needs the whole blob
	// locally before it can answer, and a plain GET is streamed through while it
	// is written to the cache.
	if r.Method == http.MethodHead {
		size, err := up.StatBlob(ctx, ref, digest)
		if err != nil {
			s.writeUpstreamError(w, "BLOB_UNKNOWN", "上游没有该 blob", err)
			return
		}
		s.countBlob(false)
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Docker-Content-Digest", digest)
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Header.Get("Range") != "" || r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Match") != "" {
		body, _, err := up.OpenBlob(ctx, ref, digest)
		if err != nil {
			s.writeUpstreamError(w, "BLOB_UNKNOWN", "拉取 blob 失败", err)
			return
		}
		_, putErr := s.blobs.Put(ctx, digest, body)
		body.Close()
		if putErr != nil {
			s.log.Warn("镜像加速：缓存 blob 失败", "digest", digest, "error", putErr)
			writeRegistryError(w, http.StatusBadGateway, "UNKNOWN",
				"缓存 blob 失败: "+putErr.Error())
			return
		}
		s.countBlob(false)
		if f, size, err := s.blobs.OpenFile(digest); err == nil {
			defer f.Close()
			h := w.Header()
			h.Set("Content-Type", "application/octet-stream")
			h.Set("Docker-Content-Digest", digest)
			h.Set("Content-Length", strconv.FormatInt(size, 10))
			cw := &countingWriter{w: w}
			http.ServeContent(cw, r, "", time.Time{}, f)
			s.addBytes(cw.n)
			return
		}
		writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "缓存后的 blob 无法读取")
		return
	}

	body, size, err := up.OpenBlob(ctx, ref, digest)
	if err != nil {
		s.writeUpstreamError(w, "BLOB_UNKNOWN", "拉取 blob 失败", err)
		return
	}
	defer body.Close()

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Docker-Content-Digest", digest)
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	counter := &countingWriter{w: w}
	n, putErr := s.blobs.Put(ctx, digest, io.TeeReader(body, counter))
	s.addBytes(counter.n)
	s.countBlob(false)
	if putErr != nil {
		// The bytes are already on the wire; the client notices the truncation
		// and retries, the cache stays consistent because Put is all or nothing.
		s.log.Warn("镜像加速：边下边存失败", "digest", digest, "written", n, "error", putErr)
	}
}

type countingWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countingWriter) Header() http.Header { return c.w.Header() }

func (c *countingWriter) WriteHeader(status int) { c.w.WriteHeader(status) }

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) readCached(digest string) ([]byte, bool) {
	rc, err := s.blobs.Open(digest)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, manifestLimit))
	if err != nil || len(body) == 0 {
		return nil, false
	}
	return body, true
}

func (s *Server) lookupTag(key string, ttlMinutes int) (tagEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tags[key]
	if !ok {
		return tagEntry{}, false
	}
	ttl := time.Duration(ttlMinutes) * time.Minute
	if ttl <= 0 {
		ttl = time.Hour
	}
	if time.Since(entry.fetched) > ttl {
		delete(s.tags, key)
		return tagEntry{}, false
	}
	return entry, true
}

func (s *Server) storeTag(key, digest, mediaType string) {
	s.mu.Lock()
	s.tags[key] = tagEntry{digest: digest, mediaType: mediaType, fetched: time.Now()}
	s.mu.Unlock()
}

func (s *Server) countManifest(hit bool) {
	s.mu.Lock()
	if hit {
		s.stats.ManifestHits++
	} else {
		s.stats.ManifestMisses++
	}
	s.mu.Unlock()
}

func (s *Server) countBlob(hit bool) {
	s.mu.Lock()
	if hit {
		s.stats.BlobHits++
	} else {
		s.stats.BlobMisses++
	}
	s.mu.Unlock()
}

func (s *Server) addBytes(n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.stats.BytesServed += n
	s.mu.Unlock()
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, code, prefix string, err error) {
	status := http.StatusNotFound
	if !isNotFound(err) {
		status = http.StatusBadGateway
	}
	s.log.Warn("镜像加速："+prefix, "error", err)
	writeRegistryError(w, status, code, prefix+": "+err.Error())
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func serveManifestBytes(w http.ResponseWriter, r *http.Request, digest, mediaType string, body []byte) {
	if mediaType == "" {
		mediaType = mediaTypeOf(body)
	}
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Docker-Content-Digest", digest)
	h.Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

func mediaTypeOf(body []byte) string {
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "application/octet-stream"
	}
	if probe.MediaType != "" {
		return probe.MediaType
	}
	if len(probe.Manifests) > 0 {
		return registry.MediaTypeOCIIndex
	}
	return registry.MediaTypeOCIManifest
}

func writeRegistryError(w http.ResponseWriter, status int, code, msg string) {
	payload := map[string]any{
		"errors": []map[string]string{{"code": code, "message": msg}},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
