package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Manifest and layer media types understood by this client.
const (
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerConfig       = "application/vnd.docker.container.image.v1+json"
	MediaTypeOCIConfig          = "application/vnd.oci.image.config.v1+json"
	MediaTypeDockerLayerGzip    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaTypeDockerLayer        = "application/vnd.docker.image.rootfs.diff.tar"
	MediaTypeOCILayerGzip       = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCILayer           = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeOCILayerZstd       = "application/vnd.oci.image.layer.v1.tar+zstd"
)

var manifestAccept = []string{
	MediaTypeOCIIndex,
	MediaTypeDockerManifestList,
	MediaTypeOCIManifest,
	MediaTypeDockerManifest,
}

// descriptor is a content-addressed object reference inside a manifest.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type indexEntry struct {
	descriptor
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform"`
}

// manifestDoc is the union of image manifest and image index documents.
type manifestDoc struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
	Manifests     []indexEntry `json:"manifests"`
}

type fetchedManifest struct {
	doc       manifestDoc
	mediaType string
	digest    string
}

func (m *fetchedManifest) isIndex() bool {
	switch m.mediaType {
	case MediaTypeOCIIndex, MediaTypeDockerManifestList:
		return true
	}
	return len(m.doc.Manifests) > 0 && m.doc.Config.Digest == ""
}

// Resolve fetches the manifest for ref, follows an image index to the manifest
// matching platform, and returns the image description. It does not download
// any layers.
func (c *Client) Resolve(ctx context.Context, ref Ref, platform string) (*Image, error) {
	p, err := ParsePlatform(platform)
	if err != nil {
		return nil, err
	}
	first, err := c.fetchManifest(ctx, ref, ref.Identifier())
	if err != nil {
		return nil, err
	}

	doc := first.doc
	manifestDigest := first.digest
	mediaType := first.mediaType
	if first.isIndex() {
		entry, err := selectPlatform(first.doc.Manifests, p)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", ref, err)
		}
		second, err := c.fetchManifest(ctx, ref, entry.Digest)
		if err != nil {
			return nil, err
		}
		if second.isIndex() {
			return nil, fmt.Errorf("image %s: nested image index at %s is not supported", ref, entry.Digest)
		}
		if second.digest != entry.Digest {
			return nil, fmt.Errorf("image %s: manifest digest mismatch: index says %s, registry returned %s", ref, entry.Digest, second.digest)
		}
		doc = second.doc
		manifestDigest = second.digest
		mediaType = second.mediaType
	}

	if doc.Config.Digest == "" {
		return nil, fmt.Errorf("image %s: manifest has no config descriptor", ref)
	}
	if !validDigest(doc.Config.Digest) {
		return nil, fmt.Errorf("image %s: unsupported config digest %q", ref, doc.Config.Digest)
	}
	switch doc.Config.MediaType {
	case MediaTypeDockerConfig, MediaTypeOCIConfig, "":
	default:
		return nil, fmt.Errorf("image %s: unsupported config media type %q", ref, doc.Config.MediaType)
	}

	img := &Image{
		Ref:            ref,
		Platform:       p.String(),
		ManifestDigest: manifestDigest,
		MediaType:      mediaType,
		ConfigDigest:   doc.Config.Digest,
		ConfigSize:     doc.Config.Size,
		Layers:         make([]Layer, 0, len(doc.Layers)),
	}
	for i, l := range doc.Layers {
		if !validDigest(l.Digest) {
			return nil, fmt.Errorf("image %s: layer %d has unsupported digest %q", ref, i, l.Digest)
		}
		if err := checkLayerMediaType(l.MediaType); err != nil {
			return nil, fmt.Errorf("image %s: layer %d: %w", ref, i, err)
		}
		img.Layers = append(img.Layers, Layer{Digest: l.Digest, Size: l.Size, MediaType: l.MediaType})
	}
	return img, nil
}

// fetchManifest GETs one manifest by tag or digest.
func (c *Client) fetchManifest(ctx context.Context, ref Ref, identifier string) (*fetchedManifest, error) {
	u := c.url(ref, "/v2/"+ref.Repository+"/manifests/"+identifier)
	resp, err := c.do(ctx, ref, u, manifestAccept)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("image %s: read manifest %s: %w", ref, identifier, err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("image %s: manifest %s not found in %s", ref, identifier, ref.Endpoint())
		}
		return nil, fmt.Errorf("image %s: fetch manifest %s: %s: %s", ref, identifier, resp.Status, snippet(body))
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("image %s: empty manifest %s", ref, identifier)
	}

	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if h := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); h != "" && h != digest {
		return nil, fmt.Errorf("image %s: manifest digest mismatch: registry says %s, content is %s", ref, h, digest)
	}
	if ref.Digest != "" && digest != ref.Digest {
		return nil, fmt.Errorf("image %s: manifest content digest %s does not match reference", ref, digest)
	}

	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("image %s: decode manifest %s: %w", ref, identifier, err)
	}
	mediaType := doc.MediaType
	if mediaType == "" {
		ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		mediaType = strings.TrimSpace(ct)
	}
	return &fetchedManifest{doc: doc, mediaType: mediaType, digest: digest}, nil
}

// selectPlatform picks the image index entry for the requested platform.
func selectPlatform(entries []indexEntry, want Platform) (indexEntry, error) {
	var available []string
	for _, e := range entries {
		osName := e.Platform.OS
		arch := e.Platform.Architecture
		if osName == "" && arch == "" {
			continue // attestation or other non-image entry
		}
		available = append(available, platformString(e.Platform.OS, arch, e.Platform.Variant))
		if osName != want.OS || arch != want.Arch {
			continue
		}
		if want.Variant != "" {
			if e.Platform.Variant == want.Variant {
				return e, nil
			}
			continue
		}
		if e.Platform.Variant == "" || e.Platform.Variant == "v8" && want.Arch == "arm64" {
			return e, nil
		}
	}
	// Accept any matching os/arch if the exact variant was not found.
	for _, e := range entries {
		if e.Platform.OS == want.OS && e.Platform.Architecture == want.Arch && e.Platform.Variant == "" {
			return e, nil
		}
	}
	sort.Strings(available)
	if len(available) == 0 {
		return indexEntry{}, fmt.Errorf("no platform specific manifests in image index")
	}
	return indexEntry{}, fmt.Errorf("platform %s not available; image provides: %s", want, strings.Join(available, ", "))
}

func platformString(osName, arch, variant string) string {
	s := osName + "/" + arch
	if variant != "" {
		s += "/" + variant
	}
	return s
}

// checkLayerMediaType accepts the layer formats we know how to decompress.
func checkLayerMediaType(mt string) error {
	switch mt {
	case MediaTypeDockerLayerGzip, MediaTypeDockerLayer,
		MediaTypeOCILayerGzip, MediaTypeOCILayer, MediaTypeOCILayerZstd:
		return nil
	case "":
		return fmt.Errorf("layer has no media type")
	}
	if strings.Contains(mt, "foreign") || strings.Contains(mt, "nondistributable") {
		return fmt.Errorf("non-distributable layer media type %q is not supported", mt)
	}
	return fmt.Errorf("unsupported layer media type %q", mt)
}

// validDigest reports whether d looks like a sha256 digest this client can verify.
func validDigest(d string) bool {
	if !strings.HasPrefix(d, "sha256:") {
		return false
	}
	h := d[len("sha256:"):]
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
