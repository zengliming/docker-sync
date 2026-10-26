package api

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
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
	block := s.tlsBlock()
	scheme := "http"
	if enabled, _ := block["enabled"].(bool); enabled || r.TLS != nil {
		scheme = "https"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":        cfg.Enabled,
		"allowAll":       cfg.AllowAll,
		"upstreams":      cfg.Upstreams,
		"tagTtlMinutes":  cfg.TagTTLMinutes,
		"localAddresses": mirrorLocalAddresses(r),
		"scheme":         scheme,
		"tls":            block,
		"stats":          stats,
	})
}

// tlsBlock describes the served certificate so the UI can print precise client
// instructions (host:port plus where to drop ca.crt).
func (s *Server) tlsBlock() map[string]any {
	if s.tls == nil || !s.tls.Enabled {
		return map[string]any{"enabled": false}
	}
	block := map[string]any{
		"enabled":  true,
		"mode":     s.tls.Mode,
		"sans":     s.tls.SANs,
		"notAfter": s.tls.NotAfter,
	}
	if len(s.tls.CAPEM) > 0 {
		block["caUrl"] = "/api/mirror/ca.crt"
	}
	return block
}

// handleMirrorCA hands out the generated CA so the operator can install it as
// /etc/docker/certs.d/<host:port>/ca.crt on the machines that pull images.
func (s *Server) handleMirrorCA(w http.ResponseWriter, r *http.Request) {
	if s.tls == nil || len(s.tls.CAPEM) == 0 {
		writeError(w, http.StatusNotFound, "当前没有可下载的 CA：HTTPS 未启用或证书由外部提供")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="docker-sync-ca.crt"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.tls.CAPEM)
}

// mirrorLocalAddresses lists the addresses a docker client can use to reach this
// mirror. The address the browser itself was opened with wins; when that is a
// loopback name (localhost / 127.0.0.1) the local non-loopback IPv4 addresses are
// added instead, because localhost only works on the very same machine.
func mirrorLocalAddresses(r *http.Request) []string {
	port := "8080"
	if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "" {
		port = p
	}
	seen := make(map[string]bool, 4)
	out := make([]string, 0, 4)
	add := func(addr string) {
		if addr == "" || seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, addr)
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !loopbackHost(host) {
		add(r.Host)
	}

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip := ipnet.IP.To4()
				if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
					continue
				}
				add(net.JoinHostPort(ip.String(), port))
			}
		}
	}

	if len(out) == 0 {
		add(r.Host)
	}
	sort.Strings(out)
	return out
}

func loopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.TrimSpace(host), ".")
	if h == "" {
		return true
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
