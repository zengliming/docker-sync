// Package registry implements a Docker Registry HTTP API V2 / OCI distribution
// client. It does not shell out to docker and does not need access to a docker
// daemon: manifests and blobs are fetched directly over HTTPS, which is what
// allows this service to run in a container without the host docker socket.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Ref is a parsed image reference.
type Ref struct {
	Registry   string // host as written by the user, e.g. docker.io, ghcr.io, registry.local:5000
	Repository string // e.g. library/alpine
	Tag        string // empty when Digest is used
	Digest     string // e.g. sha256:... or empty
}

// String renders the canonical reference, e.g. docker.io/library/alpine:3.20.
func (r Ref) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	return s + ":" + r.Tag
}

// HostRef renders repo:tag without the registry part.
func (r Ref) HostRef() string {
	if r.Digest != "" {
		return r.Repository + "@" + r.Digest
	}
	return r.Repository + ":" + r.Tag
}

// Endpoint returns the HTTP API host used to reach the registry.
func (r Ref) Endpoint() string {
	switch r.Registry {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "registry-1.docker.io"
	default:
		return r.Registry
	}
}

// Identifier returns the manifest reference used in URLs: the digest when the
// reference is pinned, otherwise the tag.
func (r Ref) Identifier() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

var (
	tagRe  = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	repoRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
)

// Parse parses an image reference such as "alpine:3.20",
// "ghcr.io/owner/app:v1", "registry.local:5000/team/app" or
// "docker.io/library/busybox@sha256:<64 hex>".
func Parse(ref string) (Ref, error) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return Ref{}, errors.New("image reference is empty")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return Ref{}, fmt.Errorf("invalid image reference %q: contains whitespace", ref)
	}
	s = strings.TrimPrefix(s, "docker://")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")

	var digest string
	if i := strings.Index(s, "@"); i >= 0 {
		s, digest = s[:i], s[i+1:]
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			return Ref{}, fmt.Errorf("invalid image reference %q: unsupported digest %q", ref, digest)
		}
	}

	name := s
	var tag string
	if digest == "" {
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name, tag = name[:i], name[i+1:]
			if tag == "" {
				return Ref{}, fmt.Errorf("invalid image reference %q: empty tag", ref)
			}
			if !tagRe.MatchString(tag) {
				return Ref{}, fmt.Errorf("invalid image reference %q: bad tag %q", ref, tag)
			}
		}
	}

	registry := "docker.io"
	repository := name
	if i := strings.Index(name, "/"); i >= 0 {
		first := name[:i]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			registry = first
			repository = name[i+1:]
		}
	}
	switch registry {
	case "index.docker.io", "registry-1.docker.io":
		registry = "docker.io"
	}
	if registry == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	if repository == "" {
		return Ref{}, fmt.Errorf("invalid image reference %q: missing repository", ref)
	}
	if registry == "docker.io" || registry == "ghcr.io" || registry == "quay.io" || registry == "gcr.io" {
		repository = strings.ToLower(repository)
	}
	if !repoRe.MatchString(repository) {
		return Ref{}, fmt.Errorf("invalid image reference %q: bad repository %q (must be lowercase)", ref, repository)
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return Ref{Registry: registry, Repository: repository, Tag: tag, Digest: digest}, nil
}

// Platform is an OS/architecture/variant tuple such as linux/amd64 or
// linux/arm/v7.
type Platform struct {
	OS      string
	Arch    string
	Variant string
}

// ParsePlatform parses "linux/amd64", "linux/arm64", "linux/arm/v7" etc.
// An empty string returns the default platform linux/amd64.
func ParsePlatform(s string) (Platform, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		s = "linux/amd64"
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return Platform{}, fmt.Errorf("invalid platform %q: expected os/arch[/variant]", s)
	}
	p := Platform{OS: parts[0], Arch: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	if p.OS == "" || p.Arch == "" {
		return Platform{}, fmt.Errorf("invalid platform %q", s)
	}
	return p, nil
}

// String renders the platform as os/arch[/variant].
func (p Platform) String() string {
	s := p.OS + "/" + p.Arch
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Layer is one filesystem layer of an image.
type Layer struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
}

// Image is the resolved description of one image for one platform.
type Image struct {
	Ref            Ref
	Platform       string
	ManifestDigest string // digest of the platform specific manifest
	MediaType      string
	ConfigDigest   string
	ConfigSize     int64
	Layers         []Layer
}

// BlobSink receives downloaded blobs. *cas.Store implements it.
type BlobSink interface {
	Has(digest string) bool
	Open(digest string) (io.ReadCloser, error)
	Put(ctx context.Context, digest string, r io.Reader) (int64, error)
}

// Options configures a Client.
type Options struct {
	Username   string
	Password   string
	Insecure   bool // talk plain HTTP to the registry
	UserAgent  string
	HTTPClient *http.Client
	// MaxConcurrentDownloads limits parallel blob downloads (default 4).
	MaxConcurrentDownloads int
}

// Client is a registry API client. It is safe for concurrent use.
type Client struct {
	opts   Options
	http   *http.Client
	mu     sync.Mutex
	tokens map[string]token
	basic  map[string]string
}

type token struct {
	value   string
	expires time.Time
}

// New creates a registry client.
func New(opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 0, Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 2 * time.Second,
		}}
	}
	if opts.MaxConcurrentDownloads <= 0 {
		opts.MaxConcurrentDownloads = 4
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "docker-sync/1.0"
	}
	return &Client{opts: opts, http: opts.HTTPClient, tokens: map[string]token{}, basic: map[string]string{}}
}

// DownloadStats reports the outcome of a Download call.
type DownloadStats struct {
	// BytesDownloaded is the sum of the sizes of blobs actually transferred.
	BytesDownloaded int64
	// BlobsDownloaded is the number of blobs fetched from the registry.
	BlobsDownloaded int
	// BlobsReused is the number of blobs that were already present in the sink.
	BlobsReused int
}
