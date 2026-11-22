package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const officialSub2Repo = "Wei-Shaw/sub2api"

// The version prefix is the official tag integrated into this fork; the YosHub
// suffix belongs only to the independent self-update channel.
func integratedUpstreamTag(version string) string {
	tag := "v" + strings.TrimPrefix(strings.TrimPrefix(version, "image-"), "v")
	if releaseVersion(tag) != "" {
		tag, _, _ = strings.Cut(tag, "-yoshub.")
	}
	if stableUpstreamTag(tag) {
		return tag
	}
	return ""
}

func stableUpstreamTag(tag string) bool {
	return semver.IsValid(tag) && semver.Prerelease(tag) == "" && semver.Canonical(tag) == tag
}

// UpstreamReleaseNotice is informational. It contains no downloadable assets.
type UpstreamReleaseNotice struct {
	BaseVersion   string `json:"base_version,omitempty"`
	LatestVersion string `json:"latest_version,omitempty"`
	Status        string `json:"status"` // current, available, unavailable
}

type upstreamReleaseCache struct {
	mu        sync.Mutex
	notice    UpstreamReleaseNotice
	checkedAt time.Time
}

func (s *UpdateService) CheckUpstreamRelease(ctx context.Context, force bool) UpstreamReleaseNotice {
	cache := &s.upstreamNotice
	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Only admin readers of this notice share the lock. Fetch failures back off
	// briefly; reopening the menu or repeatedly clicking refresh cannot flood GitHub.
	ttl := time.Duration(updateCacheTTL) * time.Second
	if force || cache.notice.Status == "unavailable" {
		ttl = 30 * time.Second
	}
	if time.Since(cache.checkedAt) < ttl {
		return cache.notice
	}
	notice := UpstreamReleaseNotice{BaseVersion: integratedUpstreamTag(s.currentVersion), Status: "unavailable"}
	if notice.BaseVersion != "" && ctx.Err() == nil {
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		release, err := s.githubClient.FetchLatestRelease(fetchCtx, officialSub2Repo)
		if err == nil && release != nil && !release.Draft && !release.Prerelease && stableUpstreamTag(release.TagName) {
			notice.LatestVersion = release.TagName
			notice.Status = "current"
			if semver.Compare(release.TagName, notice.BaseVersion) > 0 {
				notice.Status = "available"
			}
		}
	}
	if ctx.Err() == nil {
		cache.notice, cache.checkedAt = notice, time.Now()
	}
	return notice
}
