package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// Download fetches the image config and all layers into sink. Blobs already
// present in the sink are reused without transferring them again. progress, if
// non-nil, is called after each blob finishes with the number of finished
// blobs, the total blob count and the blob that finished. It may be called
// from multiple goroutines; calls are serialized.
func (c *Client) Download(ctx context.Context, img *Image, sink BlobSink, progress func(done, total int, layer Layer)) (DownloadStats, error) {
	if img == nil {
		return DownloadStats{}, errors.New("registry: nil image")
	}
	if sink == nil {
		return DownloadStats{}, errors.New("registry: nil blob sink")
	}
	blobs := make([]descriptor, 0, len(img.Layers)+1)
	blobs = append(blobs, descriptor{Digest: img.ConfigDigest, Size: img.ConfigSize})
	for _, l := range img.Layers {
		blobs = append(blobs, descriptor{Digest: l.Digest, Size: l.Size, MediaType: l.MediaType})
	}
	for i, d := range blobs {
		if !validDigest(d.Digest) {
			return DownloadStats{}, fmt.Errorf("registry: blob %d has unsupported digest %q", i, d.Digest)
		}
	}
	total := len(blobs)
	workers := c.opts.MaxConcurrentDownloads
	if workers > total {
		workers = total
	}
	if workers < 1 {
		workers = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan descriptor)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var progressMu sync.Mutex
	var stats DownloadStats
	var firstErr error
	done := 0

	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}

	worker := func() {
		defer wg.Done()
		for d := range jobs {
			if ctx.Err() != nil {
				return
			}
			reused := sink.Has(d.Digest)
			var n int64
			var err error
			if !reused {
				n, err = c.downloadBlobRetry(ctx, img.Ref, d, sink)
				if err != nil {
					fail(fmt.Errorf("image %s: %w", img.Ref, err))
					return
				}
			}
			mu.Lock()
			if reused {
				stats.BlobsReused++
			} else {
				stats.BlobsDownloaded++
				stats.BytesDownloaded += n
			}
			done++
			finished := done
			mu.Unlock()
			if progress != nil {
				progressMu.Lock()
				progress(finished, total, Layer{Digest: d.Digest, Size: d.Size, MediaType: d.MediaType})
				progressMu.Unlock()
			}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}

feed:
	for _, d := range blobs {
		select {
		case jobs <- d:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return stats, firstErr
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}

// downloadBlobRetry retries transient blob transfer failures. A blob is written
// through the sink, which verifies its digest, so retrying is safe.
func (c *Client) downloadBlobRetry(ctx context.Context, ref Ref, d descriptor, sink BlobSink) (int64, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return 0, err
			}
		}
		n, retryable, err := c.downloadBlob(ctx, ref, d, sink)
		if err == nil {
			return n, nil
		}
		if !retryable || ctx.Err() != nil {
			return n, err
		}
		lastErr = err
	}
	return 0, lastErr
}

// downloadBlob fetches one blob and stores it in the sink. The returned bool
// reports whether the failure looks transient and worth retrying.
func (c *Client) downloadBlob(ctx context.Context, ref Ref, d descriptor, sink BlobSink) (int64, bool, error) {
	u := c.url(ref, "/v2/"+ref.Repository+"/blobs/"+d.Digest)
	resp, err := c.do(ctx, ref, u, nil)
	if err != nil {
		return 0, true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return 0, false, fmt.Errorf("blob %s not found in %s", d.Digest, ref.Endpoint())
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			return 0, true, fmt.Errorf("blob %s: %s: %s", d.Digest, resp.Status, snippet(body))
		default:
			return 0, false, fmt.Errorf("blob %s: %s: %s", d.Digest, resp.Status, snippet(body))
		}
	}

	n, err := sink.Put(ctx, d.Digest, resp.Body)
	if err != nil {
		return n, true, fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	if d.Size > 0 && n != d.Size {
		return n, true, fmt.Errorf("blob %s: size mismatch: expected %d bytes, got %d", d.Digest, d.Size, n)
	}
	return n, false, nil
}
