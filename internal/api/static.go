package api

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// assetRef matches every /static/... reference inside the embedded index.html.
var assetRef = regexp.MustCompile(`(/static/[A-Za-z0-9._-]+)`)

// assetName extracts the file name from a /static/... reference.
var assetName = regexp.MustCompile(`/static/([A-Za-z0-9._-]+)$`)

// withAssetVersions appends a content hash to every /static/... reference of the
// embedded index.html. The page itself is served with "no-cache", but a browser
// (or a reverse proxy in front of the service) may still keep an older app.js,
// which used to hide newly added UI behind stale JavaScript. Versioning the URL
// makes every rebuild fetch fresh assets and lets them be cached forever.
func withAssetVersions(index []byte, web fs.FS) []byte {
	return assetRef.ReplaceAllFunc(index, func(ref []byte) []byte {
		m := assetName.FindSubmatch(ref)
		if len(m) != 2 {
			return ref
		}
		data, err := fs.ReadFile(web, path.Join("static", string(m[1])))
		if err != nil {
			return ref
		}
		sum := sha256.Sum256(data)
		return []byte(fmt.Sprintf("%s?v=%x", ref, sum[:5]))
	})
}

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
	// Versioned URLs (index.html adds ?v=<content hash>) can be cached forever,
	// plain ones must be revalidated so a stale cache cannot pin old assets.
	if r.URL.Query().Has("v") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.StripPrefix("/static/", http.FileServerFS(s.static)).ServeHTTP(w, r)
}
