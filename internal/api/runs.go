package api

import (
	"net/http"
	"strconv"

	"docker-sync/internal/model"
)

// parseLimit reads the limit query parameter, clamped to [1, max] and falling
// back to def for missing or malformed values.
func parseLimit(r *http.Request, def, max int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// handleListRuns lists runs across all tasks, newest first.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.store.ListRuns(parseLimit(r, 50, 200), "")
	if runs == nil {
		runs = []model.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleGetRun returns one run including its log lines.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.store.GetRun(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "运行记录不存在: %s", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, run)
}
