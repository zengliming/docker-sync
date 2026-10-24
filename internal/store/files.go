package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"docker-sync/internal/model"
)

// tarNameRe is the strict whitelist for artifact file names exposed through
// /api/files/{name}. Path separators and anything exotic are rejected.
var tarNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)

// ValidTarName reports whether name is an acceptable tar artifact name.
func ValidTarName(name string) bool { return validTarName(name) }

func validTarName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if filepath.Base(name) != name {
		return false
	}
	return tarNameRe.MatchString(name)
}

// tarPath resolves a validated artifact name inside tars/.
func (s *Store) tarPath(name string) string {
	return filepath.Join(s.tarsDir, name)
}

// TarPath returns the absolute path of an artifact. The caller should validate
// name with ValidTarName first.
func (s *Store) TarPath(name string) string { return s.tarPath(name) }

// HasTar reports whether the artifact exists on disk.
func (s *Store) HasTar(name string) bool {
	if !validTarName(name) {
		return false
	}
	info, err := os.Stat(s.tarPath(name))
	return err == nil && info.Mode().IsRegular()
}

// TarFile looks up one artifact index entry.
func (s *Store) TarFile(name string) (TarFile, bool) {
	s.mu.RLock()
	f, ok := s.files[name]
	s.mu.RUnlock()
	return f, ok
}

// PutTarFile records a freshly generated artifact.
func (s *Store) PutTarFile(f TarFile) {
	if !validTarName(f.Name) {
		s.log.Warn("ignoring tar index entry with invalid name", "name", f.Name)
		return
	}
	if f.ModTime.IsZero() {
		f.ModTime = time.Now()
	}
	s.mu.Lock()
	s.files[f.Name] = f
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "file", ID: f.Name, Message: "created"})
}

// ListFiles returns the artifact index as API file items, newest first.
func (s *Store) ListFiles() []model.FileItem {
	s.mu.RLock()
	items := make([]model.FileItem, 0, len(s.files))
	for _, f := range s.files {
		items = append(items, model.FileItem{
			Name:     f.Name,
			Size:     f.Size,
			ModTime:  f.ModTime,
			TaskID:   f.TaskID,
			TaskName: f.TaskName,
			Image:    f.Image,
			Digest:   f.Digest,
			URL:      "/api/files/" + f.Name,
		})
	}
	s.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].ModTime.Equal(items[j].ModTime) {
			return items[i].Name > items[j].Name
		}
		return items[i].ModTime.After(items[j].ModTime)
	})
	return items
}

// DeleteTarFile removes an artifact from disk and from the index.
func (s *Store) DeleteTarFile(name string) error {
	if !validTarName(name) {
		return fmt.Errorf("store: invalid file name %q", name)
	}
	s.mu.Lock()
	_, ok := s.files[name]
	s.mu.Unlock()
	if !ok {
		if !s.HasTar(name) {
			return fmt.Errorf("store: tar artifact %q not found", name)
		}
	}
	if err := removeFile(s.tarPath(name)); err != nil {
		return fmt.Errorf("store: remove %s: %w", name, err)
	}
	s.mu.Lock()
	delete(s.files, name)
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "file", ID: name, Message: "deleted"})
	return nil
}

// PruneTaskTars keeps the newest keep artifacts of a task and deletes the
// rest (disk and index). It returns the removed file names.
func (s *Store) PruneTaskTars(taskID string, keep int) []string {
	if keep < 1 {
		keep = 1
	}
	s.mu.RLock()
	entries := make([]TarFile, 0, 4)
	for _, f := range s.files {
		if f.TaskID == taskID {
			entries = append(entries, f)
		}
	}
	s.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModTime.After(entries[j].ModTime) })
	if len(entries) <= keep {
		return nil
	}
	removed := make([]string, 0, len(entries)-keep)
	for _, f := range entries[keep:] {
		if err := removeFile(s.tarPath(f.Name)); err != nil {
			s.log.Warn("cannot remove old tar artifact", "task", taskID, "file", f.Name, "err", err)
			continue
		}
		removed = append(removed, f.Name)
	}
	if len(removed) > 0 {
		s.mu.Lock()
		for _, name := range removed {
			delete(s.files, name)
		}
		s.mu.Unlock()
		s.markDirty()
		s.emit(model.Event{Type: "file", Message: "pruned"})
	}
	return removed
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
