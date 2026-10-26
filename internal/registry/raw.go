package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// RawManifest is one manifest document exactly as the upstream served it.
type RawManifest struct {
	MediaType string
	Digest    string
	Body      []byte
}

// RawManifest fetches a manifest by tag or digest without following an image
// index and without choosing a platform. A pull-through cache has to hand the
// index document to its own client unchanged so that the client keeps picking
// the platform it actually wants.
func (c *Client) RawManifest(ctx context.Context, ref Ref, identifier string) (RawManifest, error) {
	m, err := c.fetchManifest(ctx, ref, identifier)
	if err != nil {
		return RawManifest{}, err
	}
	return RawManifest{MediaType: m.mediaType, Digest: m.digest, Body: m.raw}, nil
}

// OpenBlob starts streaming one blob from the registry. The reader is tied to
// the response body and must be closed by the caller. size is -1 when the
// upstream does not advertise Content-Length.
func (c *Client) OpenBlob(ctx context.Context, ref Ref, digest string) (io.ReadCloser, int64, error) {
	if !validDigest(digest) {
		return nil, 0, fmt.Errorf("registry: unsupported blob digest %q", digest)
	}
	u := c.url(ref, "/v2/"+ref.Repository+"/blobs/"+digest)
	resp, err := c.do(ctx, ref, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, 0, fmt.Errorf("registry: blob %s not found in %s", digest, ref.Endpoint())
		}
		return nil, 0, fmt.Errorf("registry: blob %s: %s: %s", digest, resp.Status, snippet(body))
	}
	return resp.Body, resp.ContentLength, nil
}

// StatBlob reports whether the upstream has the blob and how large it is. It is
// used for HEAD requests against a cold cache, where the payload must not be
// transferred. HEAD costs nothing; registries that reject it degrade to a GET
// that is aborted as soon as the headers are in.
func (c *Client) StatBlob(ctx context.Context, ref Ref, digest string) (int64, error) {
	if !validDigest(digest) {
		return 0, fmt.Errorf("registry: unsupported blob digest %q", digest)
	}
	u := c.url(ref, "/v2/"+ref.Repository+"/blobs/"+digest)
	resp, err := c.doMethod(ctx, http.MethodHead, ref, u, nil)
	if err != nil {
		return 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		size := resp.ContentLength
		drainAndClose(resp)
		if size >= 0 {
			return size, nil
		}
		return c.statBlobByGet(ctx, ref, digest)
	case http.StatusNotFound:
		drainAndClose(resp)
		return 0, fmt.Errorf("registry: blob %s not found in %s", digest, ref.Endpoint())
	case http.StatusMethodNotAllowed, http.StatusBadRequest, http.StatusForbidden:
		drainAndClose(resp)
		return c.statBlobByGet(ctx, ref, digest)
	}
	drainAndClose(resp)
	return 0, fmt.Errorf("registry: stat blob %s: %s", digest, resp.Status)
}

// statBlobByGet opens the blob and aborts the transfer right after the headers.
func (c *Client) statBlobByGet(ctx context.Context, ref Ref, digest string) (int64, error) {
	u := c.url(ref, "/v2/"+ref.Repository+"/blobs/"+digest)
	resp, err := c.do(ctx, ref, u, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return 0, fmt.Errorf("registry: blob %s not found in %s", digest, ref.Endpoint())
		}
		return 0, fmt.Errorf("registry: stat blob %s: %s", digest, resp.Status)
	}
	if resp.ContentLength < 0 {
		return 0, fmt.Errorf("registry: stat blob %s: upstream did not send a length", digest)
	}
	return resp.ContentLength, nil
}
