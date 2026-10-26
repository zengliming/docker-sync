package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-sync/internal/cas"
	"docker-sync/internal/model"
	"docker-sync/internal/registry"
)

// fakeUpstream serves canned manifests and blobs and records the calls.
type fakeUpstream struct {
	mu         sync.Mutex
	manifests  map[string]registry.RawManifest
	blobs      map[string][]byte
	manifestOK []string
	blobOK     []string
	statOK     []string
	hosts      []string
	repos      []string
	err        error
}

func (f *fakeUpstream) RawManifest(_ context.Context, ref registry.Ref, identifier string) (registry.RawManifest, error) {
	f.mu.Lock()
	f.manifestOK = append(f.manifestOK, identifier)
	f.hosts = append(f.hosts, ref.Registry)
	f.repos = append(f.repos, ref.Repository)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return registry.RawManifest{}, err
	}
	m, ok := f.manifests[identifier]
	if !ok {
		return registry.RawManifest{}, fmt.Errorf("registry: image %s: manifest %s not found in %s",
			ref, identifier, ref.Endpoint())
	}
	return m, nil
}

func (f *fakeUpstream) OpenBlob(_ context.Context, _ registry.Ref, digest string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	f.blobOK = append(f.blobOK, digest)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	body, ok := f.blobs[digest]
	if !ok {
		return nil, 0, fmt.Errorf("registry: blob %s not found", digest)
	}
	return io.NopCloser(strings.NewReader(string(body))), int64(len(body)), nil
}

func (f *fakeUpstream) StatBlob(_ context.Context, _ registry.Ref, digest string) (int64, error) {
	f.mu.Lock()
	f.statOK = append(f.statOK, digest)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	body, ok := f.blobs[digest]
	if !ok {
		return 0, fmt.Errorf("registry: blob %s not found", digest)
	}
	return int64(len(body)), nil
}

func (f *fakeUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.manifestOK) + len(f.blobOK) + len(f.statOK)
}

type fakeUpstreams struct{ up *fakeUpstream }

func (f fakeUpstreams) Upstream(string) (Upstream, error) { return f.up, nil }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newTestServer(t *testing.T, up *fakeUpstream, cfg model.MirrorSettings) (*Server, *cas.Store) {
	t.Helper()
	blobs, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	settings := model.Settings{Mirror: cfg}
	srv, err := New(Options{
		Settings:  func() model.Settings { return settings },
		Blobs:     blobs,
		Upstreams: fakeUpstreams{up: up},
	})
	if err != nil {
		t.Fatalf("mirror.New: %v", err)
	}
	return srv, blobs
}

func enabledMirror() model.MirrorSettings {
	return model.MirrorSettings{
		Enabled:       true,
		Upstreams:     []string{"docker.io"},
		TagTTLMinutes: 60,
	}
}

func do(srv *Server, method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestVersionEndpoint(t *testing.T) {
	up := &fakeUpstream{}
	srv, _ := newTestServer(t, up, enabledMirror())

	for _, path := range []string{"/v2/", "/v2"} {
		rec := do(srv, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Docker-Distribution-Api-Version"); got != "registry/2.0" {
			t.Fatalf("%s: api version header = %q", path, got)
		}
		if body := strings.TrimSpace(rec.Body.String()); body != "{}" {
			t.Fatalf("%s: body = %q", path, body)
		}
	}

	disabled, _ := newTestServer(t, up, model.MirrorSettings{Upstreams: []string{"docker.io"}, TagTTLMinutes: 60})
	rec := do(disabled, http.MethodGet, "/v2/", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d, want 404", rec.Code)
	}
}

func TestManifestIndexPassThroughAndCache(t *testing.T) {
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:aa","platform":{"os":"linux","architecture":"amd64"}}]}`)
	digest := digestOf(index)
	up := &fakeUpstream{manifests: map[string]registry.RawManifest{
		"3.20": {MediaType: registry.MediaTypeOCIIndex, Digest: digest, Body: index},
	}}
	srv, blobs := newTestServer(t, up, enabledMirror())

	rec := do(srv, http.MethodGet, "/v2/library/alpine/manifests/3.20", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(index) {
		t.Fatalf("body was not passed through unchanged")
	}
	if got := rec.Header().Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, digest)
	}
	if got := rec.Header().Get("Content-Type"); got != registry.MediaTypeOCIIndex {
		t.Fatalf("Content-Type = %q", got)
	}
	if !blobs.Has(digest) {
		t.Fatalf("manifest was not cached")
	}

	// Second request is served from the cache: the manifest body is reused and
	// the tag map keeps the upstream untouched.
	rec = do(srv, http.MethodGet, "/v2/library/alpine/manifests/3.20", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != string(index) {
		t.Fatalf("second request failed: %d %q", rec.Code, rec.Body.String())
	}
	if len(up.manifestOK) != 1 {
		t.Fatalf("upstream manifest calls = %d, want 1", len(up.manifestOK))
	}
	stats := srv.Stats()
	if stats.ManifestMisses != 1 || stats.ManifestHits != 1 {
		t.Fatalf("stats = %+v, want 1 miss and 1 hit", stats)
	}
}

func TestManifestByDigestUsesCache(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:bb"}}`)
	digest := digestOf(body)
	up := &fakeUpstream{manifests: map[string]registry.RawManifest{
		digest: {MediaType: registry.MediaTypeOCIManifest, Digest: digest, Body: body},
	}}
	srv, _ := newTestServer(t, up, enabledMirror())

	first := do(srv, http.MethodGet, "/v2/library/alpine/manifests/"+digest, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", first.Code, first.Body.String())
	}
	if len(up.manifestOK) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(up.manifestOK))
	}

	second := do(srv, http.MethodGet, "/v2/library/alpine/manifests/"+digest, nil)
	if second.Code != http.StatusOK || second.Body.String() != string(body) {
		t.Fatalf("cached request failed: %d %q", second.Code, second.Body.String())
	}
	if len(up.manifestOK) != 1 {
		t.Fatalf("upstream was called again for a cached digest")
	}
	if got := second.Header().Get("Content-Type"); got != registry.MediaTypeOCIManifest {
		t.Fatalf("Content-Type = %q, want manifest media type", got)
	}
}

func TestTagCacheExpires(t *testing.T) {
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	digest := digestOf(index)
	up := &fakeUpstream{manifests: map[string]registry.RawManifest{
		"latest": {MediaType: registry.MediaTypeOCIIndex, Digest: digest, Body: index},
	}}
	cfg := enabledMirror()
	cfg.TagTTLMinutes = 1
	srv, _ := newTestServer(t, up, cfg)

	do(srv, http.MethodGet, "/v2/library/alpine/manifests/latest", nil)

	srv.mu.Lock()
	for key, entry := range srv.tags {
		entry.fetched = time.Now().Add(-2 * time.Minute)
		srv.tags[key] = entry
	}
	srv.mu.Unlock()

	do(srv, http.MethodGet, "/v2/library/alpine/manifests/latest", nil)
	if len(up.manifestOK) != 2 {
		t.Fatalf("upstream calls = %d, want 2 after the tag cache expired", len(up.manifestOK))
	}

	srv.Invalidate()
	srv.mu.Lock()
	size := len(srv.tags)
	srv.mu.Unlock()
	if size != 0 {
		t.Fatalf("Invalidate left %d tag entries", size)
	}
}

func TestBlobStreamsAndCaches(t *testing.T) {
	layer := strings.Repeat("layerdata", 512)
	digest := digestOf([]byte(layer))
	up := &fakeUpstream{blobs: map[string][]byte{digest: []byte(layer)}}
	srv, blobs := newTestServer(t, up, enabledMirror())
	target := "/v2/library/alpine/blobs/" + digest

	rec := do(srv, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != layer {
		t.Fatalf("streamed %d bytes, want %d", rec.Body.Len(), len(layer))
	}
	if got := rec.Header().Get("Content-Length"); got != "4608" {
		t.Fatalf("Content-Length = %q, want 4608", got)
	}
	if !blobs.Has(digest) {
		t.Fatalf("blob was not written to the cache")
	}

	rec = do(srv, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != layer {
		t.Fatalf("cached read failed: %d", rec.Code)
	}
	if len(up.blobOK) != 1 {
		t.Fatalf("upstream blob calls = %d, want 1", len(up.blobOK))
	}
	stats := srv.Stats()
	if stats.BlobHits != 1 || stats.BlobMisses != 1 || stats.BytesServed != int64(2*len(layer)) {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestBlobRangeFillsCacheFirst(t *testing.T) {
	layer := strings.Repeat("0123456789", 100)
	digest := digestOf([]byte(layer))
	up := &fakeUpstream{blobs: map[string][]byte{digest: []byte(layer)}}
	srv, blobs := newTestServer(t, up, enabledMirror())
	target := "/v2/library/alpine/blobs/" + digest

	h := http.Header{"Range": []string{"bytes=0-9"}}
	rec := do(srv, http.MethodGet, target, h)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "0123456789" {
		t.Fatalf("range body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-9/1000" {
		t.Fatalf("Content-Range = %q", got)
	}
	if !blobs.Has(digest) {
		t.Fatalf("ranged request did not fill the cache")
	}
}

func TestBlobHeadColdDoesNotDownload(t *testing.T) {
	layer := strings.Repeat("head", 10)
	digest := digestOf([]byte(layer))
	up := &fakeUpstream{blobs: map[string][]byte{digest: []byte(layer)}}
	srv, blobs := newTestServer(t, up, enabledMirror())
	target := "/v2/library/alpine/blobs/" + digest

	rec := do(srv, http.MethodHead, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != "40" {
		t.Fatalf("Content-Length = %q, want 40", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD returned a body of %d bytes", rec.Body.Len())
	}
	if len(up.statOK) != 1 {
		t.Fatalf("StatBlob calls = %d, want 1", len(up.statOK))
	}
	if len(up.blobOK) != 0 {
		t.Fatalf("HEAD downloaded the blob")
	}
	if blobs.Has(digest) {
		t.Fatalf("HEAD wrote the blob into the cache")
	}
}

func TestUpstreamRouting(t *testing.T) {
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	digest := digestOf(index)
	up := &fakeUpstream{manifests: map[string]registry.RawManifest{
		"latest": {MediaType: registry.MediaTypeOCIIndex, Digest: digest, Body: index},
	}}

	// Explicit host in the request path is honoured when it is allow-listed.
	cfg := enabledMirror()
	cfg.Upstreams = []string{"docker.io", "ghcr.io"}
	srv, _ := newTestServer(t, up, cfg)
	do(srv, http.MethodGet, "/v2/ghcr.io/owner/app/manifests/latest", nil)
	if len(up.hosts) != 1 || up.hosts[0] != "ghcr.io" || up.repos[0] != "owner/app" {
		t.Fatalf("routing = %v %v", up.hosts, up.repos)
	}

	// Without allowAll an unknown first segment stays part of the repository and
	// is resolved against the default upstream.
	up2 := &fakeUpstream{manifests: map[string]registry.RawManifest{
		"latest": {MediaType: registry.MediaTypeOCIIndex, Digest: digest, Body: index},
	}}
	srv2, _ := newTestServer(t, up2, enabledMirror())
	do(srv2, http.MethodGet, "/v2/example.com/owner/app/manifests/latest", nil)
	if up2.hosts[0] != "docker.io" || up2.repos[0] != "example.com/owner/app" {
		t.Fatalf("routing = %v %v", up2.hosts, up2.repos)
	}

	// With allowAll the first segment becomes the upstream.
	cfg3 := enabledMirror()
	cfg3.AllowAll = true
	up3 := &fakeUpstream{manifests: map[string]registry.RawManifest{
		"latest": {MediaType: registry.MediaTypeOCIIndex, Digest: digest, Body: index},
	}}
	srv3, _ := newTestServer(t, up3, cfg3)
	do(srv3, http.MethodGet, "/v2/quay.io/owner/app/manifests/latest", nil)
	if up3.hosts[0] != "quay.io" || up3.repos[0] != "owner/app" {
		t.Fatalf("routing = %v %v", up3.hosts, up3.repos)
	}
}

func TestRejectsWritesAndBadInput(t *testing.T) {
	up := &fakeUpstream{}
	srv, _ := newTestServer(t, up, enabledMirror())

	if rec := do(srv, http.MethodPost, "/v2/library/alpine/blobs/uploads/", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if rec := do(srv, http.MethodPut, "/v2/library/alpine/manifests/3.20", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/v2/library/alpine/blobs/notadigest", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad digest status = %d, want 400", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/v2/UPPER/alpine/manifests/latest", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo status = %d, want 400", rec.Code)
	}
	if rec := do(srv, http.MethodGet, "/v2/library/alpine/tags/list", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("tags list status = %d, want 404", rec.Code)
	}
	if up.calls() != 0 {
		t.Fatalf("upstream was contacted for invalid requests")
	}
}

func TestErrorBodyIsRegistryShaped(t *testing.T) {
	up := &fakeUpstream{
		manifests: map[string]registry.RawManifest{},
		blobs:     map[string][]byte{},
	}
	srv, _ := newTestServer(t, up, enabledMirror())

	rec := do(srv, http.MethodGet, "/v2/library/missing/manifests/latest", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var payload struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	if len(payload.Errors) != 1 || payload.Errors[0].Code == "" {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}
