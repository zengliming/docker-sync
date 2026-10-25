package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestProxyCarriesEveryRegistryRequest proves that a configured proxy is used
// for the whole Registry v2 conversation: token exchange, manifest fetch and
// blob download all appear in the proxy's request log, and no request needs a
// direct path from the client.
func TestProxyCarriesEveryRegistryRequest(t *testing.T) {
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := []byte("fake layer payload")
	configDigest := digestOfBytes(config)
	layerDigest := digestOfBytes(layer)

	var manifest []byte
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"token":"secret-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token",service="fake"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/library/app/manifests/1.0":
			w.Header().Set("Content-Type", MediaTypeDockerManifest)
			w.Header().Set("Docker-Content-Digest", digestOfBytes(manifest))
			_, _ = w.Write(manifest)
		case r.URL.Path == "/v2/library/app/blobs/"+configDigest:
			_, _ = w.Write(config)
		case r.URL.Path == "/v2/library/app/blobs/"+layerDigest:
			_, _ = w.Write(layer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer reg.Close()

	regURL, err := url.Parse(reg.URL)
	if err != nil {
		t.Fatalf("parse fake registry URL: %v", err)
	}
	ref := Ref{Registry: regURL.Host, Repository: "library/app", Tag: "1.0"}
	manifest, err = json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaTypeDockerManifest,
		"config": map[string]any{
			"mediaType": MediaTypeDockerConfig,
			"digest":    configDigest,
			"size":      len(config),
		},
		"layers": []map[string]any{{
			"mediaType": MediaTypeDockerLayer,
			"digest":    layerDigest,
			"size":      len(layer),
		}},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	var mu sync.Mutex
	var forwarded []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		forwarded = append(forwarded, r.Method+" "+r.URL.String())
		mu.Unlock()
		if !r.URL.IsAbs() {
			http.Error(w, "proxy expects absolute-form request URI", http.StatusBadRequest)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	client := New(Options{
		Insecure:               true, // the fake registry speaks plain HTTP
		ProxyURL:               proxy.URL,
		UserAgent:              "docker-sync-proxy-test",
		HTTPClient:             &http.Client{},
		NoProxy:                "",
		MaxConcurrentDownloads: 2,
	})

	ctx := context.Background()
	img, err := client.Resolve(ctx, ref, "linux/amd64")
	if err != nil {
		t.Fatalf("Resolve through proxy: %v", err)
	}
	if img.ConfigDigest != configDigest || len(img.Layers) != 1 || img.Layers[0].Digest != layerDigest {
		t.Fatalf("resolved image = %+v", img)
	}

	sink := &memSink{blobs: map[string][]byte{}}
	stats, err := client.Download(ctx, img, sink, nil)
	if err != nil {
		t.Fatalf("Download through proxy: %v", err)
	}
	if stats.BlobsDownloaded != 2 || stats.BytesDownloaded != int64(len(config)+len(layer)) {
		t.Fatalf("download stats = %+v", stats)
	}
	if !bytes.Equal(sink.blobs[layerDigest], layer) {
		t.Fatal("layer content was not stored through the proxy")
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(forwarded, "\n")
	for _, want := range []string{"/token", "/v2/library/app/manifests/1.0", "/v2/library/app/blobs/" + layerDigest} {
		if !strings.Contains(joined, want) {
			t.Fatalf("proxy saw no request for %s; log:\n%s", want, joined)
		}
	}
	if len(forwarded) < 4 { // 401 challenge + token + manifest + 2 blobs
		t.Fatalf("proxy handled %d requests, want at least 4:\n%s", len(forwarded), joined)
	}
}

func digestOfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type memSink struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (m *memSink) Has(digest string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[digest]
	return ok
}

func (m *memSink) Open(digest string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("no blob %s", digest)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memSink) Put(_ context.Context, digest string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[digest] = b
	return int64(len(b)), nil
}
