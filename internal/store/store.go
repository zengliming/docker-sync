// Package store owns the persisted state of the docker-sync service: tasks,
// run history, the tar artifact index and the global settings.
//
// State lives in a single JSON file inside the data directory. Writes are
// debounced (2s) and atomic (temp file + fsync + rename); a corrupted or
// unreadable state file is moved aside as state.json.broken and the service
// starts with default settings instead of refusing to boot.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"docker-sync/internal/model"
)

const (
	stateFileName       = "state.json"
	brokenStateFileName = "state.json.broken"
	tarsSubdir          = "tars"

	// MaxRunLogs is the maximum number of log lines kept per run.
	MaxRunLogs = 500
	// maxRuns bounds the persisted run history so state.json cannot grow
	// without limit; the newest finished runs are kept.
	maxRuns = 500

	stateVersion = 1
	saveDebounce = 2 * time.Second
	saveRetry    = 5 * time.Second
)

// TarFile is the index entry of one generated tar archive. The archive itself
// lives in <dataDir>/tars/<Name>.
type TarFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`

	TaskID   string `json:"taskId,omitempty"`
	TaskName string `json:"taskName,omitempty"`
	Image    string `json:"image,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// Options configures Open.
type Options struct {
	// Dir is the data directory (state.json and tars/ live inside it).
	Dir string
	// Logger receives store diagnostics; defaults to a discarded logger.
	Logger *slog.Logger
	// Notifier, when set, is called (outside locks) for every state change so
	// the API can push SSE events.
	Notifier func(model.Event)
}

// Store is the concurrency safe in-memory database of the service.
type Store struct {
	dir     string
	tarsDir string
	log     *slog.Logger

	mu       sync.RWMutex
	tasks    map[string]*model.Task
	runs     map[string]*model.Run
	files    map[string]TarFile
	settings model.Settings

	notifyMu sync.RWMutex
	notify   func(model.Event)

	saveMu  sync.Mutex
	timerMu sync.Mutex
	timer   *time.Timer
	dirty   bool
	closed  bool
}

// Open loads (or initialises) the store rooted at opts.Dir.
func Open(opts Options) (*Store, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return nil, errors.New("store: data directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: resolve data directory %q: %w", dir, err)
	}
	tarsDir := filepath.Join(abs, tarsSubdir)
	if err := os.MkdirAll(tarsDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create data directory %s: %w", tarsDir, err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	s := &Store{
		dir:      abs,
		tarsDir:  tarsDir,
		log:      logger,
		tasks:    make(map[string]*model.Task),
		runs:     make(map[string]*model.Run),
		files:    make(map[string]TarFile),
		settings: defaultSettings(),
		notify:   opts.Notifier,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the absolute data directory.
func (s *Store) Dir() string { return s.dir }

// TarsDir returns the absolute directory holding generated tar archives.
func (s *Store) TarsDir() string { return s.tarsDir }

// SetNotifier installs the SSE event sink. It may be called before Start to
// wire the API hub.
func (s *Store) SetNotifier(fn func(model.Event)) {
	s.notifyMu.Lock()
	s.notify = fn
	s.notifyMu.Unlock()
}

func (s *Store) emit(ev model.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.notifyMu.RLock()
	fn := s.notify
	s.notifyMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}

// diskState is the on-disk JSON schema.
type diskState struct {
	Version  int            `json:"version"`
	SavedAt  time.Time      `json:"savedAt"`
	Tasks    []model.Task   `json:"tasks"`
	Runs     []model.Run    `json:"runs"`
	Files    []TarFile      `json:"files"`
	Settings model.Settings `json:"settings"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, stateFileName) }

// load reads state.json. Unreadable or corrupt files are moved aside; this
// method never makes the service fail to start because of user data.
func (s *Store) load() error {
	data, err := os.ReadFile(s.statePath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.log.Info("state file not found, starting with defaults", "path", s.statePath())
		s.markDirty()
	case err != nil:
		s.log.Warn("state file unreadable, starting with defaults", "path", s.statePath(), "err", err)
		s.backupBrokenState()
		s.markDirty()
	default:
		var d diskState
		if err := json.Unmarshal(data, &d); err != nil {
			s.log.Warn("state file is corrupt, starting with defaults", "path", s.statePath(), "err", err)
			s.backupBrokenState()
			s.markDirty()
		} else {
			s.applyDiskState(&d)
		}
	}
	s.mu.Lock()
	s.reconcileTarsLocked()
	s.mu.Unlock()
	return nil
}

func (s *Store) backupBrokenState() {
	path := s.statePath()
	broken := filepath.Join(s.dir, brokenStateFileName)
	if err := os.Remove(broken); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("cannot remove previous broken state backup", "path", broken, "err", err)
	}
	if err := os.Rename(path, broken); err != nil {
		s.log.Warn("cannot back up broken state file", "path", path, "err", err)
		return
	}
	s.log.Warn("corrupt state file backed up", "path", broken)
}

func (s *Store) applyDiskState(d *diskState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.Settings.DefaultPlatform != "" || len(d.Settings.Registries) > 0 ||
		d.Settings.MaxConcurrentRuns > 0 || d.Settings.BlobCacheMaxGB > 0 {
		s.settings = d.Settings
	}
	s.sanitizeSettingsLocked()
	s.settings.DataDir = s.dir
	for i := range d.Tasks {
		t := &d.Tasks[i]
		if t.ID == "" {
			continue
		}
		sanitizeTask(t)
		s.tasks[t.ID] = t
	}
	for i := range d.Runs {
		r := &d.Runs[i]
		if r.ID == "" {
			continue
		}
		trimLogs(r)
		s.runs[r.ID] = r
	}
	for _, f := range d.Files {
		if !validTarName(f.Name) {
			continue
		}
		s.files[f.Name] = f
	}
}

// reconcileTarsLocked makes the tar index match the tars/ directory: index
// entries whose file disappeared are dropped, and tar files without an index
// entry (e.g. after a lost/corrupt state.json) are adopted.
func (s *Store) reconcileTarsLocked() {
	entries, err := os.ReadDir(s.tarsDir)
	if err != nil {
		s.log.Warn("cannot scan tar directory", "path", s.tarsDir, "err", err)
		return
	}
	onDisk := make(map[string]fs.FileInfo, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		onDisk[e.Name()] = info
	}
	for name := range s.files {
		if _, ok := onDisk[name]; !ok {
			delete(s.files, name)
		}
	}
	for name, info := range onDisk {
		f, ok := s.files[name]
		if !ok {
			s.files[name] = TarFile{Name: name, Size: info.Size(), ModTime: info.ModTime()}
			continue
		}
		f.Size = info.Size()
		f.ModTime = info.ModTime()
		s.files[name] = f
	}
}

// buildDiskStateLocked snapshots the state; the caller must hold at least a
// read lock.
func (s *Store) buildDiskStateLocked() diskState {
	settings := copySettings(s.settings)
	settings.DataDir = ""
	d := diskState{
		Version:  stateVersion,
		SavedAt:  time.Now(),
		Tasks:    make([]model.Task, 0, len(s.tasks)),
		Runs:     make([]model.Run, 0, len(s.runs)),
		Files:    make([]TarFile, 0, len(s.files)),
		Settings: settings,
	}
	for _, t := range s.tasks {
		d.Tasks = append(d.Tasks, *copyTask(t))
	}
	sort.Slice(d.Tasks, func(i, j int) bool {
		if d.Tasks[i].CreatedAt.Equal(d.Tasks[j].CreatedAt) {
			return d.Tasks[i].ID < d.Tasks[j].ID
		}
		return d.Tasks[i].CreatedAt.Before(d.Tasks[j].CreatedAt)
	})
	for _, r := range s.runs {
		d.Runs = append(d.Runs, *copyRun(r))
	}
	sort.Slice(d.Runs, func(i, j int) bool {
		if d.Runs[i].StartedAt.Equal(d.Runs[j].StartedAt) {
			return d.Runs[i].ID < d.Runs[j].ID
		}
		return d.Runs[i].StartedAt.Before(d.Runs[j].StartedAt)
	})
	for _, f := range s.files {
		d.Files = append(d.Files, f)
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Name < d.Files[j].Name })
	return d
}

// markDirty schedules a debounced flush.
func (s *Store) markDirty() { s.scheduleSave(saveDebounce) }

func (s *Store) scheduleSave(delay time.Duration) {
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if s.closed {
		return
	}
	s.dirty = true
	if s.timer == nil {
		s.timer = time.AfterFunc(delay, s.flushTimer)
	}
}

func (s *Store) flushTimer() {
	s.timerMu.Lock()
	s.timer = nil
	if s.closed || !s.dirty {
		s.timerMu.Unlock()
		return
	}
	s.dirty = false
	s.timerMu.Unlock()

	if err := s.writeState(); err != nil {
		s.log.Error("cannot persist state, will retry", "err", err)
		s.timerMu.Lock()
		s.dirty = true
		if !s.closed && s.timer == nil {
			s.timer = time.AfterFunc(saveRetry, s.flushTimer)
		}
		s.timerMu.Unlock()
	}
}

// Save flushes the state to disk immediately (cancelling the debounce timer).
func (s *Store) Save() error {
	s.timerMu.Lock()
	if s.closed {
		s.timerMu.Unlock()
		return nil
	}
	s.dirty = false
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.timerMu.Unlock()
	return s.writeState()
}

// Close flushes any pending change and stops the debounce timer. The store
// must not be used afterwards.
func (s *Store) Close() error {
	s.timerMu.Lock()
	if s.closed {
		s.timerMu.Unlock()
		return nil
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.timerMu.Unlock()
	return s.writeState()
}

// writeState atomically replaces state.json: write temp file, fsync, rename.
func (s *Store) writeState() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	st := s.buildDiskStateLocked()
	s.mu.RUnlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("store: create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("store: write temporary state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("store: fsync temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store: close temporary state file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store: chmod temporary state file: %w", err)
	}
	if err := os.Rename(tmpName, s.statePath()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("store: replace state file: %w", err)
	}
	syncDir(s.dir)
	return nil
}

// syncDir fsyncs a directory so a rename survives a crash. It is best effort:
// directory fsync is not supported on every platform (notably Windows).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// newID returns a short random identifier such as t_1a2b3c4d.
func newID(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%08x", prefix, uint32(time.Now().UnixNano()))
	}
	return prefix + hex.EncodeToString(b[:])
}
