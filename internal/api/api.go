// Package api implements the HTTP contract described in docs/API.md: the JSON
// REST endpoints, the server sent event stream and the embedded frontend.
package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"docker-sync/internal/cas"
	"docker-sync/internal/mirror"
	"docker-sync/internal/model"
	"docker-sync/internal/scheduler"
	"docker-sync/internal/store"
	"docker-sync/internal/webui"
)

// SchedulerAPI is the part of scheduler.Scheduler the HTTP layer uses.
type SchedulerAPI interface {
	TriggerSync(taskID, trigger string, force bool) (model.Run, error)
	CheckNow(ctx context.Context, taskID string) (scheduler.CheckResult, error)
	// UploadArchive copies an existing archive to the SMB share and records a run.
	UploadArchive(ctx context.Context, f store.TarFile, path string) (model.Run, error)
	TaskChanged(taskID string)
	TaskDeleted(taskID string)
	SettingsChanged()
	RunningCount() int
	Started() bool
}

// Options configures New.
type Options struct {
	Store     *store.Store
	Scheduler SchedulerAPI
	Blobs     *cas.Store
	WebUI     fs.FS
	Version   string
	StartedAt time.Time
	Logger    *slog.Logger
	// MirrorUpstreams overrides the registry client pool used by the
	// pull-through mirror. Tests inject a fake; nil means "talk to the real
	// upstreams through the store settings".
	MirrorUpstreams mirror.Upstreams
}

// Server is the HTTP front end of the service.
type Server struct {
	store *store.Store
	sched SchedulerAPI
	blobs *cas.Store

	web    fs.FS
	static fs.FS
	index  []byte

	mirror *mirror.Server

	version   string
	startedAt time.Time
	log       *slog.Logger
	hub       *Hub
}

// New builds the API server and connects the store event notifier to the SSE
// hub.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("api: store is required")
	}
	if opts.Scheduler == nil {
		return nil, errors.New("api: scheduler is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	web := opts.WebUI
	if web == nil {
		web = webui.FS()
	}
	s := &Server{
		store:     opts.Store,
		sched:     opts.Scheduler,
		blobs:     opts.Blobs,
		web:       web,
		version:   opts.Version,
		startedAt: opts.StartedAt,
		log:       logger,
	}
	if s.version == "" {
		s.version = "dev"
	}
	if s.startedAt.IsZero() {
		s.startedAt = time.Now()
	}
	if index, err := fs.ReadFile(web, "index.html"); err == nil {
		s.index = index
	} else {
		s.log.Warn("未找到前端 index.html", "error", err)
	}
	if static, err := fs.Sub(web, "static"); err == nil {
		s.static = static
	} else {
		s.log.Warn("未找到前端静态资源目录", "error", err)
	}
	s.hub = newHub(logger)
	opts.Store.SetNotifier(s.hub.Publish)
	if opts.Blobs != nil {
		upstreams := opts.MirrorUpstreams
		if upstreams == nil {
			upstreams = newUpstreamPool(opts.Store)
		}
		if m, err := mirror.New(mirror.Options{
			Settings:  opts.Store.SnapshotSettings,
			Blobs:     opts.Blobs,
			Upstreams: upstreams,
			Logger:    logger,
		}); err == nil {
			s.mirror = m
		} else {
			s.log.Warn("镜像加速站点初始化失败", "error", err)
		}
	}
	return s, nil
}

// Publish forwards an event to every SSE client.
func (s *Server) Publish(ev model.Event) { s.hub.Publish(ev) }

// Close disconnects every SSE client.
func (s *Server) Close() { s.hub.Close() }

// Handler returns the fully decorated HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("PUT /api/tasks/{id}", s.handleUpdateTask)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.handleDeleteTask)
	mux.HandleFunc("POST /api/tasks/{id}/sync", s.handleSyncTask)
	mux.HandleFunc("POST /api/tasks/{id}/check", s.handleCheckTask)
	mux.HandleFunc("GET /api/tasks/{id}/runs", s.handleTaskRuns)

	mux.HandleFunc("GET /api/runs", s.handleListRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)

	mux.HandleFunc("GET /api/files", s.handleListFiles)
	mux.HandleFunc("GET /api/files/{name}", s.handleDownloadFile)
	mux.HandleFunc("POST /api/files/{name}/smb", s.handleUploadFileSMB)
	mux.HandleFunc("DELETE /api/files/{name}", s.handleDeleteFile)

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("POST /api/settings/proxy-test", s.handleProxyTest)
	mux.HandleFunc("GET /api/mirror/stats", s.handleMirrorStats)

	mux.HandleFunc("/v2/", s.handleRegistryV2)
	mux.HandleFunc("/v2", s.handleRegistryV2)

	mux.HandleFunc("/api/", s.handleAPINotFound)

	mux.HandleFunc("GET /static/", s.handleStatic)
	mux.HandleFunc("/", s.handleFrontend)

	return withRecover(s.log, withLogging(s.log, withGzip(mux)))
}

// handleAPINotFound answers unknown /api paths with a JSON 404.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "接口不存在: %s %s", r.Method, r.URL.Path)
}

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "{\"error\":\"序列化响应失败\"}\n")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// writeError writes the contract error body {"error":"..."}.
func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON reads and decodes the request body into dst. An empty body is
// reported as io.EOF so callers can decide whether it is acceptable.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("请求体包含多余内容")
	}
	return nil
}

// withRecover converts handler panics into a JSON 500.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("HTTP 请求处理 panic",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				writeError(w, http.StatusInternalServerError, "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the response status and size for request logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status = code
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush lets the SSE handler stream through the logging wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withLogging logs one line per request: method, path, status, duration, size.
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if r.URL.Path == "/api/health" {
			level = slog.LevelDebug
		}
		log.Log(context.Background(), level, "http 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", sw.bytes,
			"durationMs", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// gzipResponseWriter compresses JSON responses on the fly.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	started bool
}

// WriteHeader decides whether this response is worth compressing. Only JSON
// responses are compressed, which keeps Range downloads and SSE untouched.
func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.started {
		return
	}
	g.started = true
	ct := g.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") || code == http.StatusNoContent {
		g.ResponseWriter.WriteHeader(code)
		return
	}
	g.Header().Del("Content-Length")
	g.Header().Set("Content-Encoding", "gzip")
	g.Header().Add("Vary", "Accept-Encoding")
	g.ResponseWriter.WriteHeader(code)
	g.gz = gzip.NewWriter(g.ResponseWriter)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.started {
		g.WriteHeader(http.StatusOK)
	}
	if g.gz == nil {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

// Flush flushes both the gzip stream and the underlying connection.
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close finishes the gzip stream if one was started.
func (g *gzipResponseWriter) Close() {
	if g.gz != nil {
		_ = g.gz.Close()
	}
}

// withGzip enables compression for clients that ask for it.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Registry v2 endpoints answer with exact Content-Length and support
		// range requests, so they bypass the gzip layer entirely.
		if strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/v2" {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}
