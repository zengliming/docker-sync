package api

import (
	"fmt"
	"net/http"
	"strings"

	"docker-sync/internal/model"
	"docker-sync/internal/registry"
)

// handleGetSettings returns the settings with passwords masked.
func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetSettings())
}

// handlePutSettings validates and stores the settings.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in model.Settings
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "解析请求体失败: %v", err)
		return
	}
	in.DataDir = ""
	if err := validateSettings(in); err != nil {
		writeError(w, http.StatusBadRequest, "参数校验失败: %v", err)
		return
	}
	saved, err := s.store.SetSettings(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存设置失败: %v", err)
		return
	}
	s.sched.SettingsChanged()
	writeJSON(w, http.StatusOK, saved)
}

// validateSettings enforces the ranges documented in the API contract.
func validateSettings(in model.Settings) error {
	platform := strings.TrimSpace(in.DefaultPlatform)
	if platform == "" {
		return fmt.Errorf("defaultPlatform 不能为空")
	}
	if _, err := registry.ParsePlatform(platform); err != nil {
		return fmt.Errorf("defaultPlatform %q 非法: %v", in.DefaultPlatform, err)
	}
	if in.MaxConcurrentRuns < 1 || in.MaxConcurrentRuns > 16 {
		return fmt.Errorf("maxConcurrentRuns 必须在 1 到 16 之间，当前 %d", in.MaxConcurrentRuns)
	}
	if in.MaxConcurrentDownloads < 1 || in.MaxConcurrentDownloads > 16 {
		return fmt.Errorf("maxConcurrentDownloads 必须在 1 到 16 之间，当前 %d", in.MaxConcurrentDownloads)
	}
	if in.BlobCacheMaxGB < 1 || in.BlobCacheMaxGB > 1024 {
		return fmt.Errorf("blobCacheMaxGB 必须在 1 到 1024 之间，当前 %d", in.BlobCacheMaxGB)
	}
	if in.BlobCacheMaxAgeDays < 1 || in.BlobCacheMaxAgeDays > 365 {
		return fmt.Errorf("blobCacheMaxAgeDays 必须在 1 到 365 之间，当前 %d", in.BlobCacheMaxAgeDays)
	}
	seen := make(map[string]struct{}, len(in.Registries))
	for i, reg := range in.Registries {
		host := strings.TrimSpace(reg.Host)
		if host == "" {
			return fmt.Errorf("registries[%d].host 不能为空", i)
		}
		if strings.Contains(host, "://") || strings.Contains(host, "/") {
			return fmt.Errorf("registries[%d].host %q 只能是主机名（可带端口），不能包含协议或路径", i, reg.Host)
		}
		key := strings.ToLower(host)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("registries 中存在重复的 host: %s", host)
		}
		seen[key] = struct{}{}
	}
	return nil
}
