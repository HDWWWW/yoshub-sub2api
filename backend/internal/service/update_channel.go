package service

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

const releaseBaseURL = "https://github.com/" + githubRepo + "/releases"

var stableYosHubTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-yoshub\.[0-9]+$`)

// Public image releases and source releases can refer to the same application version.
func releaseVersion(tag string) string {
	v := strings.TrimPrefix(tag, "image-")
	if !stableYosHubTag.MatchString(v) || !semver.IsValid(v) {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

func (r *ReleaseInfo) binaryAssets(platform string) (archive, checksum Asset, ok bool) {
	version := releaseVersion(r.TagName)
	if version == "" {
		return
	}
	name := "sub2api_" + version + "_" + platform + ".tar.gz"
	prefix := releaseBaseURL + "/download/" + r.TagName + "/"
	for _, a := range r.Assets {
		if a.DownloadURL != prefix+a.Name {
			continue
		}
		switch a.Name {
		case name:
			archive = a
		case "checksums.txt":
			checksum = a
		}
	}
	return archive, checksum, archive.Name != "" && checksum.Name != ""
}

type channelRelease struct {
	version   string
	info      *ReleaseInfo
	hasBinary bool
}

func (s *UpdateService) channelReleases(ctx context.Context) ([]channelRelease, error) {
	releases, err := s.githubClient.FetchRecentReleases(ctx, githubRepo, releasePageSize)
	if err != nil {
		return nil, err
	}
	candidates := make([]channelRelease, 0, len(releases))
	for _, r := range releases {
		if r == nil || r.Draft || r.Prerelease {
			continue
		}
		version := releaseVersion(r.TagName)
		if version == "" {
			continue
		}
		info := &ReleaseInfo{
			TagName:     r.TagName,
			Name:        r.Name,
			Body:        r.Body,
			PublishedAt: r.PublishedAt,
			HTMLURL:     releaseBaseURL + "/tag/" + r.TagName,
		}
		for _, a := range r.Assets {
			info.Assets = append(info.Assets, Asset{Name: a.Name, DownloadURL: a.BrowserDownloadURL, Size: a.Size})
		}
		_, _, hasBinary := info.binaryAssets(s.platform)
		candidates = append(candidates, channelRelease{version, info, hasBinary})
	}
	// If an image-only tag and a binary tag have the same version, prefer the
	// one that can update this platform, independent of publication order.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.version == b.version {
			return a.hasBinary && !b.hasBinary
		}
		return semver.Compare("v"+a.version, "v"+b.version) > 0
	})
	return candidates, nil
}

func (s *UpdateService) currentSemver() string {
	v := "v" + strings.TrimPrefix(strings.TrimPrefix(s.currentVersion, "image-"), "v")
	if !semver.IsValid(v) {
		return ""
	}
	if semver.Prerelease(v) == "" {
		return semver.Canonical(v) + "-yoshub.0"
	}
	return v
}

func (s *UpdateService) updateInfo(release *ReleaseInfo) (*UpdateInfo, error) {
	if release == nil || releaseVersion(release.TagName) == "" {
		return nil, fmt.Errorf("no stable YosHub release found")
	}
	current := s.currentSemver()
	if current == "" {
		return nil, fmt.Errorf("unrecognized installed version %q", s.currentVersion)
	}
	latest := releaseVersion(release.TagName)
	newer := semver.Compare(current, "v"+latest) < 0
	_, _, hasBinary := release.binaryAssets(s.platform)
	enabled := s.selfUpdateEnabled()
	return &UpdateInfo{
		CurrentVersion: s.currentVersion, LatestVersion: latest,
		HasUpdate: newer, CanUpdate: newer && enabled && hasBinary,
		SelfUpdateEnabled: enabled, BuildType: s.buildType, ReleaseInfo: release,
	}, nil
}

func (s *UpdateService) fetchLatestRelease(ctx context.Context) (*UpdateInfo, error) {
	releases, err := s.channelReleases(ctx)
	if err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("no stable YosHub release found")
	}
	return s.updateInfo(releases[0].info)
}

func (s *UpdateService) rollbackCandidates(ctx context.Context) ([]channelRelease, error) {
	releases, err := s.channelReleases(ctx)
	if err != nil {
		return nil, err
	}
	current := s.currentSemver()
	candidates := make([]channelRelease, 0, maxRollbackVersions)
	for _, r := range releases {
		if !r.hasBinary || semver.Compare("v"+r.version, current) >= 0 {
			continue
		}
		if n := len(candidates); n > 0 && candidates[n-1].version == r.version {
			continue
		}
		candidates = append(candidates, r)
		if len(candidates) == maxRollbackVersions {
			break
		}
	}
	return candidates, nil
}
