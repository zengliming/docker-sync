package scheduler

import (
	"math"
	"time"
)

// runBlobGC prunes the content addressed blob cache according to the current
// settings. Tar artifacts are never touched.
func (s *Scheduler) runBlobGC() {
	st := s.store.SnapshotSettings()
	maxAge := time.Duration(st.BlobCacheMaxAgeDays) * 24 * time.Hour
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}
	gb := st.BlobCacheMaxGB
	if gb <= 0 {
		gb = 20
	}
	maxBytes := int64(gb) * 1024 * 1024 * 1024
	if gb > math.MaxInt64/(1024*1024*1024) {
		maxBytes = math.MaxInt64
	}
	removed, freed, err := s.blobs.GC(maxAge, maxBytes)
	if err != nil {
		s.log.Warn("清理 blob 缓存失败", "error", err)
		return
	}
	if removed > 0 {
		s.log.Info("已清理 blob 缓存", "removed", removed, "freedBytes", freed)
	}
}
