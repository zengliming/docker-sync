package api

import (
	"net/http"
	"time"

	"docker-sync/internal/model"
)

// handleHealth returns the liveness probe payload.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   s.version,
		"uptimeSec": int64(time.Since(s.startedAt).Seconds()),
	})
}

// handleOverview returns the dashboard summary.
func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	tasks := s.store.ListTasks()
	overview := model.Overview{
		Version:   s.version,
		UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		DataDir:   s.store.Dir(),
		TaskCount: len(tasks),
	}
	for _, t := range tasks {
		if t.Enabled {
			overview.EnabledCount++
		}
		if t.Status == model.TaskStatusError {
			overview.ErrorCount++
		}
	}

	// Queued runs are already persisted with status running, so counting the
	// recent runs also covers runs waiting for a free worker slot.
	for _, run := range s.store.ListRuns(200, "") {
		if run.Status == model.RunStatusRunning {
			overview.RunningCount++
		}
	}

	files := s.store.ListFiles()
	overview.TarCount = len(files)
	for _, f := range files {
		overview.TarBytes += f.Size
	}

	if s.blobs != nil {
		count, bytes, err := s.blobs.Stats()
		if err != nil {
			s.log.Warn("读取 blob 缓存统计失败", "error", err)
		} else {
			overview.BlobCount = count
			overview.BlobBytes = bytes
		}
	}

	overview.RecentRuns = s.store.ListRuns(8, "")
	if overview.RecentRuns == nil {
		overview.RecentRuns = []model.Run{}
	}
	writeJSON(w, http.StatusOK, overview)
}
