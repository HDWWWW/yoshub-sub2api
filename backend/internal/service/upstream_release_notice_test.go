//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type upstreamNoticeFeed struct {
	*updateFeed
	release   *GitHubRelease
	latestErr error
	repos     []string
}

func (f *upstreamNoticeFeed) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	f.repos = append(f.repos, repo)
	return f.release, f.latestErr
}

func TestUpstreamReleaseNotice(t *testing.T) {
	for _, tc := range []struct {
		installed, tag, base, status string
		draft, prerelease            bool
	}{
		{"0.2.24-yoshub.50", "v0.2.24", "v0.2.24", "current", false, false},
		{"image-v0.2.9-yoshub.99", "v0.2.10", "v0.2.9", "available", false, false},
		{"0.3.0", "v0.2.99", "v0.3.0", "current", false, false},
		{"dev", "v0.2.24", "", "unavailable", false, false},
		{"0.2.24-yoshub.50", "v0.2.25-rc.1", "v0.2.24", "unavailable", false, false},
		{"0.2.24-yoshub.50", "v0.2.25", "v0.2.24", "unavailable", true, false},
		{"0.2.24-yoshub.50", "v0.2.25", "v0.2.24", "unavailable", false, true},
		{"0.2.24-yoshub.50", "v1", "v0.2.24", "unavailable", false, false},
	} {
		t.Run(tc.installed+"/"+tc.tag, func(t *testing.T) {
			s, fork := testUpdateService(t, tc.installed)
			feed := &upstreamNoticeFeed{updateFeed: fork, release: &GitHubRelease{TagName: tc.tag, Draft: tc.draft, Prerelease: tc.prerelease}}
			s.githubClient = feed
			got := s.CheckUpstreamRelease(context.Background(), false)
			require.Equal(t, tc.base, got.BaseVersion)
			require.Equal(t, tc.status, got.Status)
			if tc.base != "" {
				require.Equal(t, []string{"Wei-Shaw/sub2api"}, feed.repos)
			}
			if tc.status == "unavailable" {
				require.Empty(t, got.LatestVersion)
			} else {
				require.Equal(t, tc.tag, got.LatestVersion)
			}
		})
	}
}

func TestUpstreamNoticeCacheAndChannelIsolation(t *testing.T) {
	ctx := context.Background()
	s, fork := testUpdateService(t, "0.2.24-yoshub.50", testRelease("v0.2.24-yoshub.50", testArchiveName, "checksums.txt"))
	feed := &upstreamNoticeFeed{updateFeed: fork, release: &GitHubRelease{TagName: "v0.2.25"}}
	s.githubClient = feed
	var wait sync.WaitGroup
	for range 10 {
		wait.Add(1)
		go func() { defer wait.Done(); _ = s.CheckUpstreamRelease(ctx, false) }()
	}
	wait.Wait()
	require.Equal(t, []string{"Wei-Shaw/sub2api"}, feed.repos)
	info, err := s.CheckUpdate(ctx, false)
	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.ErrorIs(t, s.PerformUpdate(ctx), ErrNoUpdateAvailable)
	require.Equal(t, "HDWWWW/yoshub-sub2api", fork.repository)
	versions, err := s.ListRollbackVersions(ctx)
	require.NoError(t, err)
	require.Empty(t, versions)
	require.Equal(t, "available", s.CheckUpstreamRelease(ctx, true).Status)
	require.Len(t, feed.repos, 1)
	s.upstreamNotice.checkedAt = time.Now().Add(-31 * time.Second)
	feed.latestErr = errors.New("offline")
	require.Equal(t, "available", s.CheckUpstreamRelease(ctx, false).Status)
	got := s.CheckUpstreamRelease(ctx, true)
	require.Equal(t, "unavailable", got.Status)
	require.Empty(t, got.LatestVersion)
	require.Len(t, feed.repos, 2)
	require.Equal(t, "unavailable", s.CheckUpstreamRelease(ctx, false).Status)
	require.Len(t, feed.repos, 2)
	feed.latestErr = nil
	s.upstreamNotice.checkedAt = time.Now().Add(-31 * time.Second)
	require.Equal(t, "available", s.CheckUpstreamRelease(ctx, false).Status)
	fork.err = errors.New("fork unavailable")
	info, err = s.CheckUpdate(ctx, true)
	require.NoError(t, err)
	require.NotEmpty(t, info.Warning)
	require.Equal(t, "available", s.CheckUpstreamRelease(ctx, false).Status)
}
