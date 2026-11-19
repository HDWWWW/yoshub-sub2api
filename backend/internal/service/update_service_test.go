//go:build unit

package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct{ data string }

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) { return s.data, nil }
func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateFeed struct {
	GitHubReleaseClient
	releases   []*GitHubRelease
	files      map[string][]byte
	err        error
	repository string
}

func (f *updateFeed) FetchRecentReleases(_ context.Context, repo string, _ int) ([]*GitHubRelease, error) {
	f.repository = repo
	return f.releases, f.err
}
func (f *updateFeed) DownloadFile(_ context.Context, url, dest string, _ int64) error {
	data, ok := f.files[url]
	if !ok {
		return errors.New("missing archive")
	}
	return os.WriteFile(dest, data, 0600)
}
func (f *updateFeed) FetchChecksumFile(_ context.Context, url string) ([]byte, error) {
	data, ok := f.files[url]
	if !ok {
		return nil, errors.New("missing checksum")
	}
	return data, nil
}

func testRelease(tag string, files ...string) *GitHubRelease {
	r := &GitHubRelease{TagName: tag}
	for _, name := range files {
		name = strings.ReplaceAll(name, "{version}", releaseVersion(tag))
		r.Assets = append(r.Assets, GitHubAsset{Name: name, BrowserDownloadURL: releaseBaseURL + "/download/" + tag + "/" + name})
	}
	return r
}

const testArchiveName = "sub2api_{version}_linux_amd64.tar.gz"

func testUpdateService(t *testing.T, current string, releases ...*GitHubRelease) (*UpdateService, *updateFeed) {
	t.Helper()
	t.Setenv("SUB2API_DISABLE_SELF_UPDATE", "")
	feed := &updateFeed{releases: releases, files: map[string][]byte{}}
	svc := NewUpdateService(&updateServiceCacheStub{}, feed, current, "release")
	svc.platform = "linux_amd64"
	return svc, feed
}

func TestUpdateServiceChannelAndRollback(t *testing.T) {
	ctx := context.Background()
	full := func(tag string) *GitHubRelease { return testRelease(tag, testArchiveName, "checksums.txt") }
	draft := full("v0.2.24-yoshub.1")
	draft.Draft = true
	pre := full("v0.2.23-yoshub.101")
	pre.Prerelease = true
	offRepo := full("v0.2.23-yoshub.11")
	offRepo.Assets[0].BrowserDownloadURL = strings.Replace(offRepo.Assets[0].BrowserDownloadURL, githubRepo, "other/fork", 1)
	svc, feed := testUpdateService(t, "0.2.23-yoshub.47", testRelease("image-v0.2.23-yoshub.100"), draft, pre,
		full("v9.0.0"), full("v0.2.23-yoshub.500-rc.1"), full("v0.2.23-yoshub.100"), full("image-v0.2.23-yoshub.9"),
		full("v0.2.23-yoshub.9"), offRepo, full("image-v0.2.23-yoshub.10"), full("v0.2.23-yoshub.8"), full("v0.2.23-yoshub.7"),
		testRelease("image-v0.2.23-yoshub.46", "sub2api-v0.2.23-yoshub.46-linux-amd64.tar.gz", "SHA256SUMS"))
	info, err := svc.CheckUpdate(ctx, false)
	require.NoError(t, err)
	require.Equal(t, "HDWWWW/yoshub-sub2api", feed.repository)
	require.Equal(t, "0.2.23-yoshub.100", info.LatestVersion)
	require.Equal(t, "v0.2.23-yoshub.100", info.ReleaseInfo.TagName)
	require.True(t, info.CanUpdate)
	versions, err := svc.ListRollbackVersions(ctx)
	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "image-v0.2.23-yoshub.10", versions[0].TagName)
	require.Equal(t, "0.2.23-yoshub.9", versions[1].Version)
	require.Equal(t, "0.2.23-yoshub.8", versions[2].Version)
	for _, v := range []string{"", "0.2.23-yoshub.7", "0.2.23-yoshub.47", "0.2.23-yoshub.100", "0.2.23-yoshub.46", "0.2.23-yoshub.11"} {
		require.ErrorIs(t, svc.RollbackToVersion(ctx, v), ErrRollbackVersionNotAllowed)
	}
}

func TestUpdateServiceAvailability(t *testing.T) {
	ctx := context.Background()
	for _, assets := range [][]string{{testArchiveName}, {"sub2api-v0.2.23-yoshub.48-linux-amd64.tar.gz", "SHA256SUMS", "checksums.txt"}, {"sub2api_{version}_linux_arm64.tar.gz", "checksums.txt"}} {
		svc, _ := testUpdateService(t, "0.2.23-yoshub.47", testRelease("image-v0.2.23-yoshub.48", assets...))
		info, err := svc.CheckUpdate(ctx, false)
		require.NoError(t, err)
		require.True(t, info.HasUpdate)
		require.False(t, info.CanUpdate)
		require.ErrorIs(t, svc.PerformUpdate(ctx), ErrUpdatePackageMissing)
	}
	svc, feed := testUpdateService(t, "0.2.23-yoshub.48", testRelease("v0.2.23-yoshub.48", testArchiveName, "checksums.txt"))
	require.ErrorIs(t, svc.PerformUpdate(ctx), ErrNoUpdateAvailable)
	svc.currentVersion = "0.2.23-yoshub.9"
	feed.err = errors.New("GitHub unavailable")
	info, err := svc.CheckUpdate(ctx, true)
	require.NoError(t, err)
	require.True(t, info.Cached)
	require.True(t, info.HasUpdate)
	require.False(t, info.CanUpdate)
	require.Contains(t, info.Warning, "GitHub unavailable")
	require.ErrorIs(t, svc.PerformUpdate(ctx), feed.err)
	_, err = svc.ListRollbackVersions(ctx)
	require.ErrorIs(t, err, feed.err)
	svc.cache = &updateServiceCacheStub{}
	info, err = svc.CheckUpdate(ctx, true)
	require.NoError(t, err)
	require.Empty(t, info.LatestVersion)
	require.NotEmpty(t, info.Warning)
	feed.err = nil
	svc.buildType = "source"
	require.ErrorIs(t, svc.PerformUpdate(ctx), ErrSelfUpdateUnavailable)
	svc.buildType = "release"
	t.Setenv("SUB2API_DISABLE_SELF_UPDATE", "1")
	require.ErrorIs(t, svc.Rollback(), ErrSelfUpdateUnavailable)
}

func TestUpdateServiceInstallAndRestore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux executable replacement")
	}
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("SUB2API_UPDATE_DIR", dir)
	target := filepath.Join(dir, "sub2api")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0755))
	next := testRelease("image-v0.2.23-yoshub.48", testArchiveName, "checksums.txt")
	old := testRelease("v0.2.23-yoshub.47", testArchiveName, "checksums.txt")
	svc, feed := testUpdateService(t, "0.2.23-yoshub.47", next, old)
	for _, r := range feed.releases {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tarw := tar.NewWriter(gz)
		payload := []byte(r.TagName)
		require.NoError(t, tarw.WriteHeader(&tar.Header{Name: "sub2api", Typeflag: tar.TypeReg, Size: int64(len(payload)), Mode: 0755}))
		_, err := tarw.Write(payload)
		require.NoError(t, err)
		require.NoError(t, tarw.Close())
		require.NoError(t, gz.Close())
		feed.files[r.Assets[0].BrowserDownloadURL] = buf.Bytes()
		feed.files[r.Assets[1].BrowserDownloadURL] = []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(buf.Bytes()), r.Assets[0].Name))
	}
	assertFile := func(path, want string) {
		t.Helper()
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	}
	sumURL := next.Assets[1].BrowserDownloadURL
	valid := feed.files[sumURL]
	feed.files[sumURL] = []byte(fmt.Sprintf("%064d  %s\n", 0, next.Assets[0].Name))
	require.ErrorContains(t, svc.PerformUpdate(ctx), "checksum mismatch")
	assertFile(target, "old")
	require.NoFileExists(t, target+".backup")
	feed.files[sumURL] = valid
	require.NoError(t, svc.PerformUpdate(ctx))
	assertFile(target, next.TagName)
	assertFile(target+".backup", "old")
	require.NoError(t, svc.PerformUpdate(ctx)) // retry after staging, before restart
	assertFile(target+".backup", "old")
	svc.currentVersion = "0.2.23-yoshub.48"
	require.NoError(t, svc.RollbackToVersion(ctx, "v0.2.23-yoshub.47"))
	assertFile(target, old.TagName)
	require.NoError(t, svc.Rollback())
	assertFile(target, next.TagName)
}
