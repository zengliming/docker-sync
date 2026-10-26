// Package cas implements a small content-addressed blob store used as an
// on-disk layer cache shared by all tasks.
//
// Blobs are stored under <root>/sha256/<hex digest> and are always verified
// against their expected sha256 digest before being committed. Writes are
// atomic (temp file + rename), so a crash can never leave a corrupt blob in
// place under its final name.
package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is a content-addressed blob store.
type Store struct {
	root string
}

// New opens (and creates if needed) a blob store rooted at root.
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("cas: empty root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{abs, filepath.Join(abs, "sha256")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cas: create %s: %w", dir, err)
		}
	}
	return &Store{root: abs}, nil
}

// Root returns the absolute store root directory.
func (s *Store) Root() string { return s.root }

func digestHex(digest string) (string, error) {
	d := strings.TrimSpace(digest)
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) != 64 {
		return "", fmt.Errorf("cas: invalid sha256 digest %q", digest)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("cas: invalid sha256 digest %q: %w", digest, err)
	}
	return strings.ToLower(h), nil
}

// Path returns the on-disk path of digest.
func (s *Store) Path(digest string) (string, error) {
	h, err := digestHex(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, "sha256", h), nil
}

// Has reports whether the blob exists in the store.
func (s *Store) Has(digest string) bool {
	p, err := s.Path(digest)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Open opens the blob for reading.
func (s *Store) Open(digest string) (io.ReadCloser, error) {
	p, err := s.Path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("cas: blob %s not found: %w", digest, err)
		}
		return nil, err
	}
	return f, nil
}

// Size returns the size of the stored blob in bytes.
func (s *Store) Size(digest string) (int64, error) {
	p, err := s.Path(digest)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	return st.Size(), nil
}

// OpenFile opens the blob as a seekable file together with its size. It exists
// so HTTP handlers can serve blobs with range requests and conditional headers
// through http.ServeContent.
func (s *Store) OpenFile(digest string) (*os.File, int64, error) {
	p, err := s.Path(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("cas: blob %s not found: %w", digest, err)
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Put streams r into the store and verifies it matches digest. If the blob is
// already present the existing copy is kept (and its access time refreshed) and
// the reader is drained. It returns the blob size.
func (s *Store) Put(ctx context.Context, digest string, r io.Reader) (int64, error) {
	p, err := s.Path(digest)
	if err != nil {
		return 0, err
	}
	if s.Has(digest) {
		n, err := io.Copy(io.Discard, ctxReader{ctx: ctx, r: r})
		if err != nil {
			return 0, err
		}
		_ = touch(p)
		return n, nil
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".blob-*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx: ctx, r: r})
	if err != nil {
		return 0, fmt.Errorf("cas: write blob %s: %w", digest, err)
	}
	want, _ := digestHex(digest)
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return 0, fmt.Errorf("cas: digest mismatch for %s: got sha256:%s", digest, got)
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return 0, err
	}
	return n, nil
}

// PutBytes stores a small in-memory blob.
func (s *Store) PutBytes(ctx context.Context, digest string, data []byte) (int64, error) {
	return s.Put(ctx, digest, strings.NewReader(string(data)))
}

func touch(path string) error {
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// Stats returns the number of blobs and their total size.
func (s *Store) Stats() (int, int64, error) {
	var count int
	var total int64
	entries, err := os.ReadDir(filepath.Join(s.root, "sha256"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		total += info.Size()
	}
	return count, total, nil
}

// GC removes blobs older than maxAge and then, if the store is still larger
// than maxBytes, the least recently modified blobs until it fits. A zero
// maxAge disables age based removal; a maxBytes <= 0 disables size based removal.
func (s *Store) GC(maxAge time.Duration, maxBytes int64) (removed int, freed int64, err error) {
	type blob struct {
		path    string
		size    int64
		modTime time.Time
	}
	var blobs []blob
	var total int64
	entries, err := os.ReadDir(filepath.Join(s.root, "sha256"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		blobs = append(blobs, blob{filepath.Join(s.root, "sha256", e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].modTime.Before(blobs[j].modTime) })
	now := time.Now()
	for _, b := range blobs {
		if maxAge > 0 && now.Sub(b.modTime) > maxAge {
			if err := os.Remove(b.path); err == nil {
				removed++
				freed += b.size
				total -= b.size
			}
		}
	}
	if maxBytes > 0 && total > maxBytes {
		for _, b := range blobs {
			if total <= maxBytes {
				break
			}
			if _, statErr := os.Stat(b.path); statErr != nil {
				continue // already removed by the age pass
			}
			if err := os.Remove(b.path); err == nil {
				removed++
				freed += b.size
				total -= b.size
			}
		}
	}
	return removed, freed, nil
}
