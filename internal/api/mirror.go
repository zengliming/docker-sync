package api

import (
	"fmt"
	"net/http"
	"sync"

	"docker-sync/internal/mirror"
	"docker-sync/internal/registry"
	"docker-sync/internal/store"
)

// upstreamPool builds and caches registry clients for the mirror endpoints. A
// client is rebuilt when the settings that affect it change (credentials,
// plain HTTP, proxy), so saving the settings page takes effect immediately.
type upstreamPool struct {
	store *store.Store

	mu    sync.Mutex
	entry map[string]poolEntry
}

type poolEntry struct {
	key    string
	client *registry.Client
}

func newUpstreamPool(st *store.Store) *upstreamPool {
	return &upstreamPool{store: st, entry: make(map[string]poolEntry)}
}

// Upstream implements mirror.Upstreams.
func (p *upstreamPool) Upstream(host string) (mirror.Upstream, error) {
	if host == "" {
		return nil, fmt.Errorf("镜像加速: 上游地址为空")
	}
	st := p.store.SnapshotSettings()
	opts := registry.Options{
		MaxConcurrentDownloads: st.MaxConcurrentDownloads,
		UserAgent:              "docker-sync-mirror",
		ProxyURL:               st.ProxyURL,
		NoProxy:                st.NoProxy,
	}
	if auth, ok := p.store.RegistryAuthFor(host); ok {
		opts.Username = auth.Username
		opts.Password = auth.Password
		opts.Insecure = auth.Insecure
		if auth.ProxyURL != "" {
			opts.ProxyURL = auth.ProxyURL
		}
	}
	key := fmt.Sprintf("%t|%s|%s|%s|%s", opts.Insecure, opts.ProxyURL, opts.NoProxy, opts.Username, opts.Password)

	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entry[host]; ok && e.key == key {
		return e.client, nil
	}
	client := registry.New(opts)
	p.entry[host] = poolEntry{key: key, client: client}
	return client, nil
}

// handleRegistryV2 serves the read-only Registry v2 API used by docker clients.
func (s *Server) handleRegistryV2(w http.ResponseWriter, r *http.Request) {
	if s.mirror == nil {
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"UNKNOWN","message":"镜像加速站点未初始化（缺少 blob 缓存）"}]}`))
		return
	}
	s.mirror.Handler().ServeHTTP(w, r)
}

// handleMirrorStats reports the mirror configuration and cache counters.
func (s *Server) handleMirrorStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.SnapshotSettings().Mirror
	stats := mirror.Stats{}
	if s.mirror != nil {
		stats = s.mirror.Stats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       cfg.Enabled,
		"allowAll":      cfg.AllowAll,
		"upstreams":     cfg.Upstreams,
		"tagTtlMinutes": cfg.TagTTLMinutes,
		"stats":         stats,
	})
}
