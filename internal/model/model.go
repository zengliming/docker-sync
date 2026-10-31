// Package model contains the shared data types of the docker-sync service.
// All types in this package are serialized to JSON and are part of the HTTP API
// contract documented in docs/API.md.
package model

import "time"

// Task status values shown in the UI.
const (
	TaskStatusIdle    = "idle"
	TaskStatusQueued  = "queued"
	TaskStatusRunning = "running"
	TaskStatusError   = "error"
	TaskStatusPaused  = "paused"
)

// Run status values.
const (
	RunStatusRunning  = "running"
	RunStatusSuccess  = "success"
	RunStatusSkipped  = "skipped"
	RunStatusError    = "error"
	RunStatusCanceled = "canceled"
)

// Run triggers.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
	TriggerCheck    = "check"
	// TriggerUpload is a manual SMB upload of an archive that already exists.
	TriggerUpload = "upload"
)

// Run phases, reported in Run.Phase while the run is in progress.
const (
	PhaseQueued      = "queued"
	PhaseResolving   = "resolving"
	PhaseDownloading = "downloading"
	PhasePacking     = "packing"
	PhaseUploading   = "uploading"
	PhaseDone        = "done"
)

// Schedule types.
const (
	ScheduleInterval = "interval"
	ScheduleCron     = "cron"
)

// Log line levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Task is an image watch/sync job.
type Task struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Image is a full image reference, e.g. "alpine:3.20" or
	// "ghcr.io/owner/app:v1.2.3" or "docker.io/library/busybox@sha256:...".
	Image        string `json:"image"`
	Platform     string `json:"platform"`     // "linux/amd64" etc.
	ScheduleType string `json:"scheduleType"` // interval | cron
	// IntervalMinutes is used when ScheduleType == "interval".
	IntervalMinutes int `json:"intervalMinutes"`
	// Cron is a standard 5-field cron expression (minute hour dom month dow),
	// evaluated in the server's local time zone.
	Cron string `json:"cron"`
	// AutoSync pulls the image and rebuilds the tar automatically when a
	// scheduled check detects a new digest. When false the check only records
	// that an update is available.
	AutoSync bool `json:"autoSync"`
	// IgnoreLatest skips scheduled checks and automatic pulls for tasks whose
	// reference points at the "latest" tag, because that tag moves on every
	// upstream push. Manual syncs and manual checks still run.
	IgnoreLatest bool `json:"ignoreLatest"`
	// KeepVersions is how many tar artifacts are kept per task (>= 1).
	KeepVersions int  `json:"keepVersions"`
	Enabled      bool `json:"enabled"`

	Status    string `json:"status"` // idle | queued | running | error | paused
	LastError string `json:"lastError"`

	LastCheckedAt    *time.Time `json:"lastCheckedAt"`
	LastSyncedAt     *time.Time `json:"lastSyncedAt"`
	LastDigest       string     `json:"lastDigest"`       // platform manifest digest
	LastConfigDigest string     `json:"lastConfigDigest"` // image config digest
	// UpdateAvailable is set by a check when the remote digest differs from
	// LastDigest. It is cleared by a successful sync.
	UpdateAvailable bool   `json:"updateAvailable"`
	RemoteDigest    string `json:"remoteDigest"`

	LatestTar     string `json:"latestTar"`
	LatestTarSize int64  `json:"latestTarSize"`

	// NextRunAt is computed by the scheduler and is read-only through the API.
	NextRunAt *time.Time `json:"nextRunAt,omitempty"`
	LastRunID string     `json:"lastRunId,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TaskInput is the payload accepted by POST /api/tasks and PUT /api/tasks/{id}.
type TaskInput struct {
	Name            string `json:"name"`
	Image           string `json:"image"`
	Platform        string `json:"platform"`
	ScheduleType    string `json:"scheduleType"`
	IntervalMinutes int    `json:"intervalMinutes"`
	Cron            string `json:"cron"`
	AutoSync        bool   `json:"autoSync"`
	IgnoreLatest    bool   `json:"ignoreLatest"`
	KeepVersions    int    `json:"keepVersions"`
	Enabled         bool   `json:"enabled"`
}

// LogLine is one line of a run log.
type LogLine struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Run is one execution of a task (scheduled check, manual sync or manual check).
type Run struct {
	ID       string `json:"id"`
	TaskID   string `json:"taskId"`
	TaskName string `json:"taskName"`
	Image    string `json:"image"`
	Platform string `json:"platform"`
	Trigger  string `json:"trigger"` // manual | schedule | check | upload
	Status   string `json:"status"`  // running | success | skipped | error | canceled
	Phase    string `json:"phase"`

	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`

	UpdateAvailable bool   `json:"updateAvailable"`
	RemoteDigest    string `json:"remoteDigest"`
	PreviousDigest  string `json:"previousDigest"`
	ConfigDigest    string `json:"configDigest"`

	TarName string `json:"tarName"`
	TarSize int64  `json:"tarSize"`
	Layers  int    `json:"layers"`
	// BytesDownloaded / BlobsReused report blob cache efficiency.
	BytesDownloaded int64 `json:"bytesDownloaded"`
	BlobsReused     int   `json:"blobsReused"`

	Error string    `json:"error,omitempty"`
	Logs  []LogLine `json:"logs,omitempty"`
}

// RegistryAuth holds credentials for one registry host.
type RegistryAuth struct {
	Host     string `json:"host"` // e.g. docker.io, ghcr.io, registry.local:5000
	Username string `json:"username"`
	// Password is only accepted on write. GET /api/settings always returns an
	// empty Password and sets HasPassword instead.
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	// Insecure allows plain HTTP for this registry (e.g. a LAN registry).
	Insecure bool `json:"insecure"`
	// ProxyURL overrides the global proxy for this host only. Empty means
	// "use the global proxy". Set it to "-" to force a direct connection.
	ProxyURL string `json:"proxyUrl,omitempty"`
}

// SMBSettings configures automatic remote archive copies.
type SMBSettings struct {
	Enabled     bool   `json:"enabled"`
	Share       string `json:"share"`
	Directory   string `json:"directory"`
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	Domain      string `json:"domain"`
}

// MirrorSettings turns the service into a read-only pull-through registry cache
// that docker clients can pull from directly ("镜像加速站点").
type MirrorSettings struct {
	Enabled bool `json:"enabled"`
	// AllowAll lets the mirror proxy any upstream registry. When false only the
	// hosts listed in Upstreams are served.
	AllowAll bool `json:"allowAll"`
	// Upstreams lists the allowed upstream registry hosts. The first entry is the
	// default: requests that do not start with a configured host are resolved
	// against it (docker.io by default).
	Upstreams []string `json:"upstreams"`
	// TagTTLMinutes is how long a tag -> manifest digest mapping is reused before
	// the upstream is asked whether the tag moved.
	TagTTLMinutes int `json:"tagTtlMinutes"`
}

// Settings is the global service configuration.
type Settings struct {
	SMB                    SMBSettings    `json:"smb"`
	Mirror                 MirrorSettings `json:"mirror"`
	DefaultPlatform        string         `json:"defaultPlatform"`
	MaxConcurrentRuns      int            `json:"maxConcurrentRuns"`
	MaxConcurrentDownloads int            `json:"maxConcurrentDownloads"`
	BlobCacheMaxGB         int            `json:"blobCacheMaxGB"`
	BlobCacheMaxAgeDays    int            `json:"blobCacheMaxAgeDays"`
	// ProxyURL is the outbound proxy for registry requests, e.g.
	// "http://127.0.0.1:7890" or "socks5://127.0.0.1:7890". Empty means
	// "use the HTTP_PROXY/HTTPS_PROXY environment variables".
	ProxyURL string `json:"proxyUrl,omitempty"`
	// NoProxy is a comma separated list of hosts that must bypass ProxyURL,
	// e.g. "docker.io,*.local". The docker.io spelling also covers
	// registry-1.docker.io and index.docker.io.
	NoProxy    string         `json:"noProxy,omitempty"`
	Registries []RegistryAuth `json:"registries"`
	// DataDir is read-only and reported for display purposes only.
	DataDir string `json:"dataDir,omitempty"`
}

// FileItem describes a tar artifact on disk.
type FileItem struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"modTime"`
	TaskID   string    `json:"taskId,omitempty"`
	TaskName string    `json:"taskName,omitempty"`
	Image    string    `json:"image,omitempty"`
	Digest   string    `json:"digest,omitempty"`
	// URL is the download link, e.g. /api/files/<name>.
	URL string `json:"url"`
}

// Overview is the dashboard summary returned by GET /api/overview.
type Overview struct {
	Version      string `json:"version"`
	UptimeSec    int64  `json:"uptimeSec"`
	DataDir      string `json:"dataDir"`
	TaskCount    int    `json:"taskCount"`
	EnabledCount int    `json:"enabledCount"`
	RunningCount int    `json:"runningCount"`
	ErrorCount   int    `json:"errorCount"`
	TarCount     int    `json:"tarCount"`
	TarBytes     int64  `json:"tarBytes"`
	BlobCount    int    `json:"blobCount"`
	BlobBytes    int64  `json:"blobBytes"`
	RecentRuns   []Run  `json:"recentRuns"`
}

// Event is published on the SSE stream /api/events.
type Event struct {
	Type string `json:"type"` // task | run | file | log | settings
	ID   string `json:"id,omitempty"`
	// Run logs are sent as type "log" with the run ID in ID.
	Message string    `json:"message,omitempty"`
	Time    time.Time `json:"time"`
}
