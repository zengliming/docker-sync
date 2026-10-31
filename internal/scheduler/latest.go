package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"docker-sync/internal/model"
	"docker-sync/internal/registry"
)

// versionTagPattern matches the concrete version tags a latest-ignoring task is
// allowed to follow: an optional leading "v" plus one to four numeric
// components, so "3.20", "1.27.4" and "v2.1" qualify while "latest", "stable",
// "3.20-alpine" and "edge" do not.
var versionTagPattern = regexp.MustCompile(`^[vV]?(\d+(?:\.\d+){0,3})$`)

// pickNewestTag returns the highest version among tags. Only plain numeric
// versions participate, which keeps pre-release and flavour tags
// (2.0.0-rc1, 3.20-alpine, latest) out of the automatic follow. Tags with two
// or more numeric components win over single numbers, so the release line
// (3.22.1) beats a date-stamped snapshot tag (20260805) even though its number
// is larger.
func pickNewestTag(tags []string) (string, error) {
	if best, _, ok := newestVersion(tags, true); ok {
		return best, nil
	}
	if best, _, ok := newestVersion(tags, false); ok {
		return best, nil
	}
	return "", fmt.Errorf("上游共 %d 个标签，但没有形如 1.2.3 的版本标签", len(tags))
}

// newestVersion scans tags for the highest version, optionally restricted to
// multi-component versions.
func newestVersion(tags []string, multiOnly bool) (string, []int, bool) {
	best := ""
	var bestParts []int
	for _, tag := range tags {
		parts, ok := parseVersionTag(tag)
		if !ok {
			continue
		}
		if multiOnly && len(parts) < 2 {
			continue
		}
		if best == "" || compareVersionParts(parts, bestParts) > 0 {
			best, bestParts = tag, parts
		}
	}
	return best, bestParts, best != ""
}

// parseVersionTag splits a numeric version tag into its components.
func parseVersionTag(tag string) ([]int, bool) {
	match := versionTagPattern.FindStringSubmatch(strings.TrimSpace(tag))
	if match == nil {
		return nil, false
	}
	fields := strings.Split(match[1], ".")
	parts := make([]int, 0, len(fields))
	for _, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil {
			return nil, false
		}
		parts = append(parts, value)
	}
	return parts, true
}

// compareVersionParts compares two version component lists, treating missing
// trailing components as zero so 3.20 and 3.20.0 rank equal.
func compareVersionParts(a, b []int) int {
	for i := 0; i < 4; i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// latestTarget rewrites the reference of a task that opted out of the moving
// "latest" tag to the newest concrete version published upstream. Tasks with an
// explicit tag or digest are returned untouched.
func (s *Scheduler) latestTarget(ctx context.Context, task model.Task, ref registry.Ref) (registry.Ref, error) {
	if !ignoredLatestTag(task) {
		return ref, nil
	}
	listCtx, cancel := context.WithTimeout(ctx, runResolveTimeout)
	tags, err := s.clientFor(ref.Registry).ListTags(listCtx, ref)
	cancel()
	if err != nil {
		return ref, fmt.Errorf("%w: 列出 %s 的标签失败: %w", ErrUpstream, ref.Repository, err)
	}
	tag, err := pickNewestTag(tags)
	if err != nil {
		return ref, fmt.Errorf("%w: 无法确定 %s 的最新版本: %w", ErrUpstream, ref.Repository, err)
	}
	ref.Tag = tag
	ref.Digest = ""
	return ref, nil
}

// resolvedTagFor records which concrete tag the task actually followed: only
// latest-ignoring tasks need it, everything else tracks an explicit version.
func resolvedTagFor(task model.Task, ref registry.Ref) string {
	if !ignoredLatestTag(task) {
		return ""
	}
	return ref.Tag
}

// repoTagFor picks the repository tag recorded inside the tar. A latest-following
// task is tagged with the concrete version it synced (alpine:3.24.3), so
// "docker load" never produces a moving latest tag; everything else keeps the
// configured reference.
func repoTagFor(task model.Task, ref registry.Ref) string {
	if ref.Tag == "" || ref.Digest != "" {
		return task.Image
	}
	return ref.String()
}
