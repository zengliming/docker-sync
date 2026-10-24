package api

import (
	"bytes"
	"net/http"
	"path"
	"strings"
)

// handleFrontend serves index.html for "/" and for unknown non /api paths so a
// hash router never sees a 404 on refresh.
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "方法不允许: %s", r.Method)
		return
	}
	if len(s.index) == 0 {
		writeError(w, http.StatusNotFound, "前端页面未内嵌")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", s.startedAt, bytes.NewReader(s.index))
}

// handleStatic serves /static/* from the embedded web/static directory.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.static == nil {
		writeError(w, http.StatusNotFound, "静态资源不存在: %s", r.URL.Path)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		writeError(w, http.StatusNotFound, "静态资源不存在: %s", r.URL.Path)
		return
	}
	// Prevent directory listings: only regular files below the root are served.
	if name != path.Clean(name) {
		writeError(w, http.StatusNotFound, "静态资源不存在: %s", r.URL.Path)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.StripPrefix("/static/", http.FileServerFS(s.static)).ServeHTTP(w, r)
}
