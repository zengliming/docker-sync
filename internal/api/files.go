package api

import (
	"errors"
	"net/http"
	"os"

	"docker-sync/internal/model"
	"docker-sync/internal/store"
)

// handleListFiles lists every generated tar artifact, newest first.
func (s *Server) handleListFiles(w http.ResponseWriter, _ *http.Request) {
	files := s.store.ListFiles()
	if files == nil {
		files = []model.FileItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleDownloadFile streams a tar artifact and supports Range requests.
func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !store.ValidTarName(name) {
		writeError(w, http.StatusBadRequest, "非法文件名: %q", name)
		return
	}
	if _, ok := s.store.TarFile(name); !ok {
		writeError(w, http.StatusNotFound, "文件不存在: %s", name)
		return
	}
	f, err := os.Open(s.store.TarPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "文件不存在: %s", name)
			return
		}
		writeError(w, http.StatusInternalServerError, "读取文件 %s 失败: %v", name, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取文件 %s 失败: %v", name, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleDeleteFile removes a tar artifact from disk and from the index.
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !store.ValidTarName(name) {
		writeError(w, http.StatusBadRequest, "非法文件名: %q", name)
		return
	}
	if _, ok := s.store.TarFile(name); !ok {
		writeError(w, http.StatusNotFound, "文件不存在: %s", name)
		return
	}
	if err := s.store.DeleteTarFile(name); err != nil {
		writeError(w, http.StatusInternalServerError, "删除文件 %s 失败: %v", name, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
