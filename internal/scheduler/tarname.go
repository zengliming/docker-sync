package scheduler

import (
	"regexp"
	"strings"

	"docker-sync/internal/registry"
)

// TarName builds the artifact name of one built archive:
// <repo 下划线>_<tag>_<platform 下划线>_<manifest digest 前 12 位>.tar
// e.g. library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar
func TarName(ref registry.Ref, platform, manifestDigest string) string {
	repo := strings.ReplaceAll(ref.Repository, "/", "_")
	repo = sanitizeNamePart(repo)
	tag := ref.Tag
	if ref.Digest != "" {
		tag = ""
	}
	tag = sanitizeNamePart(tag)
	plat := sanitizeNamePart(strings.ReplaceAll(platform, "/", "_"))
	digest := ""
	if idx := strings.Index(manifestDigest, ":"); idx >= 0 {
		digest = manifestDigest[idx+1:]
	} else {
		digest = manifestDigest
	}
	if len(digest) > 12 {
		digest = digest[:12]
	}
	parts := []string{repo}
	if tag != "" {
		parts = append(parts, tag)
	}
	parts = append(parts, plat, digest)
	name := strings.Join(parts, "_") + ".tar"
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}

// namePartRe matches the characters allowed inside an artifact name.
var namePartRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeNamePart replaces every character outside the whitelist with "_".
func sanitizeNamePart(part string) string {
	part = strings.TrimSpace(part)
	if part == "" {
		return ""
	}
	cleaned := strings.Trim(namePartRe.ReplaceAllString(part, "_"), "._-")
	if cleaned == "" {
		return "x"
	}
	return cleaned
}
