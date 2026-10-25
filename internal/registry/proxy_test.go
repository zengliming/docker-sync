package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseProxy(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "-", want: "-"},
		{in: "http://127.0.0.1:7890", want: "http://127.0.0.1:7890"},
		{in: "127.0.0.1:7890", want: "http://127.0.0.1:7890"},
		{in: "localhost:7890", want: "http://localhost:7890"},
		{in: "https://proxy.example.com", want: "https://proxy.example.com"},
		{in: "socks5://127.0.0.1:7891", want: "socks5://127.0.0.1:7891"},
		{in: "socks5h://127.0.0.1:7891", want: "socks5h://127.0.0.1:7891"},
		{in: "socks4://127.0.0.1:7891", want: "socks4://127.0.0.1:7891"},
		{in: "ftp://127.0.0.1", wantErr: true},
		{in: "socks4a://127.0.0.1", wantErr: true},
		{in: "http://", wantErr: true},
		{in: "http://:7890", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseProxy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseProxy(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProxy(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseProxy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBypassProxy(t *testing.T) {
	entries := parseNoProxy("docker.io, *.corp.local, 10.0.0.1:5000, .internal")
	cases := []struct {
		host string
		want bool
	}{
		{"docker.io", true},
		{"registry-1.docker.io", true}, // Docker Hub spellings fold together
		{"DOCKER.IO", true},
		{"api.corp.local", true},
		{"corp.local", true},
		{"10.0.0.1", false}, // the entry pins :5000, so it only matches that port
		{"10.0.0.1:5000", true},
		{"10.0.0.2", false},
		{"host.internal", true},
		{"registry.example.com", false},
	}
	for _, tc := range cases {
		if got := bypassProxy(tc.host, entries); got != tc.want {
			t.Errorf("bypassProxy(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
	if bypassProxy("anything", parseNoProxy("")) {
		t.Error("empty bypass list must not bypass anything")
	}
	if !bypassProxy("anything", parseNoProxy("*")) {
		t.Error("wildcard must bypass everything")
	}
}

func TestProxyFuncRoutes(t *testing.T) {
	proxy := "http://127.0.0.1:8899"

	direct := proxyFunc("", "")
	req := httptest.NewRequest(http.MethodGet, "https://registry-1.docker.io/v2/", nil)
	if u, err := direct(req); err != nil || u != nil {
		t.Fatalf("no proxy configured must delegate to the environment: %v %v", u, err)
	}

	never := proxyFunc("-", "")
	if u, err := never(req); err != nil || u != nil {
		t.Fatalf("dash must connect directly: %v %v", u, err)
	}

	always := proxyFunc(proxy, "")
	u, err := always(req)
	if err != nil || u == nil || u.String() != proxy {
		t.Fatalf("proxy always = %v %v", u, err)
	}

	mixed := proxyFunc(proxy, "docker.io, *.local")
	if u, err := mixed(req); err != nil || u != nil {
		t.Fatalf("bypassed host must be direct: %v %v", u, err)
	}
	other := httptest.NewRequest(http.MethodGet, "https://ghcr.io/v2/", nil)
	if u, err := mixed(other); err != nil || u == nil || u.String() != proxy {
		t.Fatalf("other host must use the proxy: %v %v", u, err)
	}
}

// TestProxyTestThroughRealProxy runs the probe against an httptest server used
// as an HTTP proxy, proving the transport honours the configured proxy.
func TestProxyTestThroughRealProxy(t *testing.T) {
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer proxy.Close()

	err := ProxyTest(context.Background(), "http://registry-1.docker.io/v2/", proxy.URL, "", 5*time.Second)
	if err != nil {
		t.Fatalf("ProxyTest through proxy: %v", err)
	}
	if proxied != 1 {
		t.Fatalf("proxy handled %d requests, want 1", proxied)
	}

	// A dead proxy must surface as a connection failure.
	err = ProxyTest(context.Background(), "http://registry-1.docker.io/v2/", "http://127.0.0.1:1", "", time.Second)
	if err == nil {
		t.Fatal("ProxyTest through a dead proxy must fail")
	}
}
