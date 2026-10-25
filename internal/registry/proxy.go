package registry

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ParseProxy validates a proxy URL and returns it in normalised form. It
// accepts the schemes used in practice for registry traffic:
//
//	http://host:port, https://host:port, socks5://host:port,
//	socks5h://host:port, socks4://host:port
//
// An empty string means "no explicit proxy" (the environment variables still
// apply). The special value "-" means "never use a proxy" and is returned
// unchanged.
func ParseProxy(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "-" {
		return s, nil
	}
	if !strings.Contains(s, "://") {
		// A bare host:port is most likely an HTTP proxy.
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("代理地址 %q 解析失败: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h", "socks4":
	case "socks4a":
		return "", fmt.Errorf("代理地址 %q 使用了不受支持的协议 socks4a（可用 http/https/socks5/socks5h/socks4）", raw)
	default:
		return "", fmt.Errorf("代理地址 %q 使用了不受支持的协议 %q（可用 http/https/socks5/socks5h/socks4）", raw, u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("代理地址 %q 缺少主机名", raw)
	}
	return u.String(), nil
}

// proxyFunc builds the transport proxy function for the client options.
//
// Behaviour:
//   - no proxy configured: honour HTTP(S)_PROXY / NO_PROXY exactly like
//     net/http does by default.
//   - proxy configured: use it for every request unless the target host
//     matches noProxy (comma separated: exact host, "*.suffix", ".suffix",
//     "host:port" or "*").
//   - proxy set to "-": always connect directly, ignoring the environment.
func proxyFunc(proxyURL, noProxy string) func(*http.Request) (*url.URL, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return http.ProxyFromEnvironment
	}
	if proxyURL == "-" {
		return func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return func(*http.Request) (*url.URL, error) {
			return nil, fmt.Errorf("代理地址 %q 无效: %w", proxyURL, err)
		}
	}
	entries := parseNoProxy(noProxy)
	return func(req *http.Request) (*url.URL, error) {
		if bypassProxy(req.URL.Host, entries) {
			return nil, nil
		}
		return parsed, nil
	}
}

// parseNoProxy splits a comma separated bypass list into lower-case entries.
func parseNoProxy(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// bypassProxy reports whether hostport must be reached without a proxy.
//
// Entries are matched case-insensitively; an entry with a port only matches
// that exact port, while an entry without a port matches every port. The
// Docker Hub spellings docker.io, index.docker.io and registry-1.docker.io are
// treated as the same host.
func bypassProxy(hostport string, entries []string) bool {
	host, port := splitHostPortLoose(strings.ToLower(strings.TrimSpace(hostport)))
	if host == "" {
		return false
	}
	host = canonicalBypassHost(host)
	for _, e := range entries {
		if e == "*" {
			return true
		}
		eh, eport := splitHostPortLoose(e)
		if eport != "" && eport != port {
			continue
		}
		eh = canonicalBypassHost(eh)
		switch {
		case strings.HasPrefix(eh, "*."):
			if host == eh[2:] || strings.HasSuffix(host, eh[1:]) {
				return true
			}
		case strings.HasPrefix(eh, "."):
			if strings.HasSuffix(host, eh) {
				return true
			}
		case eh == host:
			return true
		}
	}
	return false
}

// splitHostPortLoose splits "host:port" without failing on a missing port; it
// returns the host and an empty port when there is no port.
func splitHostPortLoose(hostport string) (string, string) {
	hostport = strings.TrimSpace(hostport)
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]"), p
	}
	return strings.Trim(hostport, "[]"), ""
}

// canonicalBypassHost folds the Docker Hub spellings into one name so that a
// "docker.io" bypass entry also covers registry-1.docker.io.
func canonicalBypassHost(host string) string {
	switch host {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return host
}

// ProxyTest performs a small GET through the given proxy configuration and
// reports whether the registry endpoint is reachable. 401/404 responses are
// treated as success: the registry answered, which is what we probe for.
func ProxyTest(ctx context.Context, targetURL, proxyURL, noProxy string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: proxyFunc(proxyURL, noProxy)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Errorf("构建探测请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "docker-sync/proxy-test")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return fmt.Errorf("代理要求认证（407）: %s", resp.Status)
	}
	return nil
}
