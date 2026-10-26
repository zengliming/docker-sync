package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"docker-sync/internal/model"
	"docker-sync/internal/registry"
	"docker-sync/internal/remote"
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
	if s.mirror != nil {
		s.mirror.Invalidate()
	}
	writeJSON(w, http.StatusOK, saved)
}

// validateSettings enforces the ranges documented in the API contract.
func validateSettings(in model.Settings) error {
	if err := remote.Validate(in.SMB); err != nil {
		return err
	}
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
	if _, err := registry.ParseProxy(in.ProxyURL); err != nil {
		return err
	}
	if err := validateNoProxy(in.NoProxy); err != nil {
		return err
	}
	for i, reg := range in.Registries {
		if _, err := registry.ParseProxy(reg.ProxyURL); err != nil {
			return fmt.Errorf("registries[%d]: %w", i, err)
		}
	}
	return validateMirrorSettings(in.Mirror)
}

// validateMirrorSettings checks the mirror upstream allow-list. Omitted fields
// are tolerated so that clients written before the mirror existed keep working:
// the store fills in the documented defaults (docker.io, 60 minutes).
func validateMirrorSettings(m model.MirrorSettings) error {
	if m.TagTTLMinutes < 0 || m.TagTTLMinutes > 1440 {
		return fmt.Errorf("mirror.tagTtlMinutes 必须在 1 到 1440 之间，当前 %d", m.TagTTLMinutes)
	}
	seen := make(map[string]struct{}, len(m.Upstreams))
	for i, raw := range m.Upstreams {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			return fmt.Errorf("mirror.upstreams[%d] 不能为空", i)
		}
		if strings.Contains(host, "://") || strings.ContainsAny(host, "/ @\\") {
			return fmt.Errorf("mirror.upstreams[%d] %q 只能是主机名（可带端口），不能包含协议、路径或空格", i, raw)
		}
		if _, dup := seen[host]; dup {
			return fmt.Errorf("mirror.upstreams 中存在重复的上游: %s", host)
		}
		seen[host] = struct{}{}
	}
	return nil
}

// validateNoProxy checks the bypass list syntax: comma separated hosts, no
// scheme and no slash.
func validateNoProxy(raw string) error {
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "://") || strings.Contains(part, "/") {
			return fmt.Errorf("noProxy 条目 %q 只能是主机名或 .suffix，不能包含协议或路径", part)
		}
	}
	return nil
}

// proxyTestRequest is the body of POST /api/settings/proxy-test.
type proxyTestRequest struct {
	ProxyURL string `json:"proxyUrl"`
	NoProxy  string `json:"noProxy"`
	// Image optionally selects the registry to probe, e.g. "ghcr.io/x/y:1".
	Image string `json:"image"`
}

// proxyTestResponse reports the probe outcome.
type proxyTestResponse struct {
	OK        bool   `json:"ok"`
	Target    string `json:"target"`
	ProxyURL  string `json:"proxyUrl,omitempty"`
	NoProxy   string `json:"noProxy,omitempty"`
	ElapsedMS int64  `json:"elapsedMs"`
	Error     string `json:"error,omitempty"`
}

// handleProxyTest probes registry connectivity through the given proxy
// configuration (or the stored one when the body omits it).
func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var in proxyTestRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "解析请求体失败: %v", err)
		return
	}
	target, err := s.proxyTestTarget(in.Image)
	if err != nil {
		writeError(w, http.StatusBadRequest, "参数校验失败: %v", err)
		return
	}
	proxyURL := strings.TrimSpace(in.ProxyURL)
	noProxy := in.NoProxy
	if proxyURL == "" && noProxy == "" {
		stored := s.store.SnapshotSettings()
		proxyURL, noProxy = stored.ProxyURL, stored.NoProxy
	}
	if _, err := registry.ParseProxy(proxyURL); err != nil {
		writeError(w, http.StatusBadRequest, "参数校验失败: %v", err)
		return
	}
	if err := validateNoProxy(noProxy); err != nil {
		writeError(w, http.StatusBadRequest, "参数校验失败: %v", err)
		return
	}
	start := time.Now()
	probeErr := registry.ProxyTest(r.Context(), target, proxyURL, noProxy, 15*time.Second)
	resp := proxyTestResponse{
		OK:        probeErr == nil,
		Target:    target,
		ProxyURL:  proxyURL,
		NoProxy:   noProxy,
		ElapsedMS: time.Since(start).Milliseconds(),
	}
	if probeErr != nil {
		msg := probeErr.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		resp.Error = msg
	}
	writeJSON(w, http.StatusOK, resp)
}

// proxyTestTarget maps an optional image reference to the registry /v2/
// endpoint used by the proxy probe.
func (s *Server) proxyTestTarget(image string) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "https://registry-1.docker.io/v2/", nil
	}
	ref, err := registry.Parse(image)
	if err != nil {
		return "", fmt.Errorf("镜像引用 %q 非法: %v", image, err)
	}
	scheme := "https"
	if auth, ok := s.store.RegistryAuthFor(ref.Registry); ok && auth.Insecure {
		scheme = "http"
	}
	return scheme + "://" + ref.Endpoint() + "/v2/", nil
}
