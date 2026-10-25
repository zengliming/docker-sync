package store

import (
	"strings"

	"docker-sync/internal/model"
)

// defaultSettings returns the documented defaults used on a fresh data dir.
func defaultSettings() model.Settings {
	return model.Settings{
		DefaultPlatform:        "linux/amd64",
		MaxConcurrentRuns:      2,
		MaxConcurrentDownloads: 4,
		BlobCacheMaxGB:         20,
		BlobCacheMaxAgeDays:    7,
		Registries:             []model.RegistryAuth{},
	}
}

// GetSettings returns the current settings. Passwords are blanked out; the
// HasPassword flag tells whether one is stored.
func (s *Store) GetSettings() model.Settings {
	s.mu.RLock()
	out := copySettings(s.settings)
	s.mu.RUnlock()
	for i := range out.Registries {
		if out.Registries[i].Password != "" {
			out.Registries[i].Password = ""
			out.Registries[i].HasPassword = true
		}
	}
	out.DataDir = s.dir
	return out
}

// SetSettings replaces the settings, preserving stored passwords for entries
// that come in with an empty password while HasPassword is true. Callers pass
// the settings exactly as received from the API layer (already validated).
func (s *Store) SetSettings(in model.Settings) (model.Settings, error) {
	s.mu.Lock()
	s.settings = mergeSettings(s.settings, in)
	s.sanitizeSettingsLocked()
	s.mu.Unlock()

	s.markDirty()
	s.emit(model.Event{Type: "settings", Message: "updated"})
	return s.GetSettings(), nil
}

// UpdateSettings mutates the stored settings through fn. It is used by the
// service itself (not the API) for cross-cutting changes.
func (s *Store) UpdateSettings(fn func(*model.Settings)) {
	s.mu.Lock()
	fn(&s.settings)
	s.sanitizeSettingsLocked()
	s.mu.Unlock()
	s.markDirty()
}

// SnapshotSettings returns the settings as stored, including passwords. Only
// internal consumers (registry client factory) may use this.
func (s *Store) SnapshotSettings() model.Settings {
	s.mu.RLock()
	out := copySettings(s.settings)
	s.mu.RUnlock()
	out.DataDir = s.dir
	return out
}

// RegistryAuthFor returns the credentials configured for a registry host. The
// docker.io host also matches the index.docker.io spelling. It reports false
// when no credentials are known, in which case anonymous access is used.
func (s *Store) RegistryAuthFor(host string) (model.RegistryAuth, bool) {
	want := normalizeRegistryHost(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.settings.Registries {
		if normalizeRegistryHost(r.Host) == want {
			return r, true
		}
	}
	return model.RegistryAuth{}, false
}

// normalizeRegistryHost canonicalises the docker hub spellings so that
// docker.io, index.docker.io and registry-1.docker.io are interchangeable.
func normalizeRegistryHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return h
}

func copySettings(in model.Settings) model.Settings {
	out := in
	out.Registries = append([]model.RegistryAuth(nil), in.Registries...)
	return out
}

func mergeSettings(cur, in model.Settings) model.Settings {
	out := copySettings(in)
	out.Registries = out.Registries[:0]
	for i := range in.Registries {
		next := in.Registries[i]
		if next.Password == "" {
			// An empty password means "keep what is stored" only when the
			// caller echoed HasPassword; otherwise it explicitly clears it.
			if next.HasPassword {
				if prev, ok := findRegistry(cur.Registries, next.Host); ok {
					next.Password = prev.Password
				}
			}
			next.HasPassword = next.Password != ""
		} else {
			next.HasPassword = true
		}
		out.Registries = append(out.Registries, next)
	}
	return out
}

func findRegistry(list []model.RegistryAuth, host string) (model.RegistryAuth, bool) {
	for _, r := range list {
		if r.Host == host {
			return r, true
		}
	}
	return model.RegistryAuth{}, false
}

// sanitizeSettingsLocked clamps the settings to the documented ranges.
func (s *Store) sanitizeSettingsLocked() {
	set := &s.settings
	if set.DefaultPlatform == "" {
		set.DefaultPlatform = "linux/amd64"
	}
	if set.MaxConcurrentRuns <= 0 {
		set.MaxConcurrentRuns = 2
	}
	if set.MaxConcurrentRuns > 16 {
		set.MaxConcurrentRuns = 16
	}
	if set.MaxConcurrentDownloads <= 0 {
		set.MaxConcurrentDownloads = 4
	}
	if set.MaxConcurrentDownloads > 16 {
		set.MaxConcurrentDownloads = 16
	}
	if set.BlobCacheMaxGB <= 0 {
		set.BlobCacheMaxGB = 20
	}
	if set.BlobCacheMaxGB > 1024 {
		set.BlobCacheMaxGB = 1024
	}
	if set.BlobCacheMaxAgeDays <= 0 {
		set.BlobCacheMaxAgeDays = 7
	}
	if set.BlobCacheMaxAgeDays > 365 {
		set.BlobCacheMaxAgeDays = 365
	}
	if set.Registries == nil {
		set.Registries = []model.RegistryAuth{}
	}
	set.ProxyURL = strings.TrimSpace(set.ProxyURL)
	set.NoProxy = strings.TrimSpace(set.NoProxy)
	for i := range set.Registries {
		set.Registries[i].Host = strings.TrimSpace(set.Registries[i].Host)
		set.Registries[i].Username = strings.TrimSpace(set.Registries[i].Username)
		set.Registries[i].ProxyURL = strings.TrimSpace(set.Registries[i].ProxyURL)
	}
}
