package service

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrSelfUpdateUnavailable     = infraerrors.Conflict("SELF_UPDATE_UNAVAILABLE", "in-place updates are unavailable for this installation")
	ErrUpdatePackageMissing      = infraerrors.Conflict("UPDATE_PACKAGE_MISSING", "the release has no verified binary package for this platform")
	ErrNoUpdateAvailable         = infraerrors.Conflict("ALREADY_UP_TO_DATE", "no update available; current version is latest")
	ErrRollbackVersionNotAllowed = infraerrors.BadRequest("ROLLBACK_VERSION_NOT_ALLOWED", "version is not in the allowed rollback list")
)

const (
	updateCacheTTL = 1200 // 20 minutes
	githubRepo     = "HDWWWW/yoshub-sub2api"

	// Security: max download size (500MB)
	maxDownloadSize = 500 * 1024 * 1024

	// Rollback: expose at most the 3 most recent versions older than current
	maxRollbackVersions = 3
	// Fetch the largest GitHub page so image-only releases do not crowd out rollback targets.
	releasePageSize = 100
)

// UpdateCache defines cache operations for update service
type UpdateCache interface {
	GetUpdateInfo(ctx context.Context) (string, error)
	SetUpdateInfo(ctx context.Context, data string, ttl time.Duration) error
}

// GitHubReleaseClient 获取 GitHub release 信息的接口
type GitHubReleaseClient interface {
	FetchLatestRelease(ctx context.Context, repo string) (*GitHubRelease, error)
	FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*GitHubRelease, error)
	DownloadFile(ctx context.Context, url, dest string, maxSize int64) error
	FetchChecksumFile(ctx context.Context, url string) ([]byte, error)
}

// UpdateService handles software updates
type UpdateService struct {
	platform       string // GOOS_GOARCH of this installation
	cache          UpdateCache
	githubClient   GitHubReleaseClient
	currentVersion string
	buildType      string // "source" for manual builds, "release" for CI builds
}

// NewUpdateService creates a new UpdateService
func NewUpdateService(cache UpdateCache, githubClient GitHubReleaseClient, version, buildType string) *UpdateService {
	return &UpdateService{
		platform:       runtime.GOOS + "_" + runtime.GOARCH,
		cache:          cache,
		githubClient:   githubClient,
		currentVersion: version,
		buildType:      buildType,
	}
}

// UpdateInfo contains update information
type UpdateInfo struct {
	SelfUpdateEnabled bool         `json:"self_update_enabled"`
	CanUpdate         bool         `json:"can_update"`
	CurrentVersion    string       `json:"current_version"`
	LatestVersion     string       `json:"latest_version"`
	HasUpdate         bool         `json:"has_update"`
	ReleaseInfo       *ReleaseInfo `json:"release_info,omitempty"`
	Cached            bool         `json:"cached"`
	Warning           string       `json:"warning,omitempty"`
	BuildType         string       `json:"build_type"` // "source" or "release"
}

// ReleaseInfo contains GitHub release details
type ReleaseInfo struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Assets      []Asset `json:"assets,omitempty"`
}

// Asset represents a release asset
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
}

// GitHubRelease represents GitHub API response
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []GitHubAsset `json:"assets"`
}

// RollbackVersion describes a release version the system can roll back to
type RollbackVersion struct {
	TagName     string `json:"tag_name"`
	Version     string `json:"version"` // without "v" prefix, e.g. "0.1.146"
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// CheckUpdate checks for available updates
func (s *UpdateService) CheckUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	// Try cache first
	if !force {
		if cached, err := s.getFromCache(ctx); err == nil && cached != nil {
			return cached, nil
		}
	}

	// Fetch from GitHub
	info, err := s.fetchLatestRelease(ctx)
	if err != nil {
		// Return cached on error
		if cached, cacheErr := s.getFromCache(ctx); cacheErr == nil && cached != nil {
			cached.Warning = "Using cached data: " + err.Error()
			cached.CanUpdate = false
			return cached, nil
		}
		return &UpdateInfo{
			CurrentVersion:    s.currentVersion,
			SelfUpdateEnabled: s.selfUpdateEnabled(),
			HasUpdate:         false,
			Warning:           err.Error(),
			BuildType:         s.buildType,
		}, nil
	}

	// Cache result
	s.saveToCache(ctx, info)
	return info, nil
}

// PerformUpdate downloads and applies the update
// Uses atomic file replacement pattern for safe in-place updates
func (s *UpdateService) PerformUpdate(ctx context.Context) error {
	if !s.selfUpdateEnabled() {
		return ErrSelfUpdateUnavailable
	}
	// Never install using stale cached release information.
	info, err := s.fetchLatestRelease(ctx)
	if err != nil {
		return err
	}
	s.saveToCache(ctx, info)
	if !info.HasUpdate {
		return ErrNoUpdateAvailable
	}
	return s.applyReleaseAssets(ctx, info.ReleaseInfo)
}

// The Docker entrypoint selects a path on the data volume. Standalone installs
// replace the program at the same path their service manager starts.
func updateTarget() (string, error) {
	if dir := os.Getenv("SUB2API_UPDATE_DIR"); dir != "" {
		return filepath.Join(dir, "sub2api"), nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func (s *UpdateService) selfUpdateEnabled() bool {
	return s.buildType == "release" && (strings.HasPrefix(s.platform, "linux_") || strings.HasPrefix(s.platform, "darwin_")) && os.Getenv("SUB2API_DISABLE_SELF_UPDATE") != "1"
}

func (s *UpdateService) applyReleaseAssets(ctx context.Context, release *ReleaseInfo) error {
	archive, checksum, ok := release.binaryAssets(s.platform)
	if !ok {
		return ErrUpdatePackageMissing
	}
	exePath, err := updateTarget()
	if err != nil {
		return err
	}
	exeDir := filepath.Dir(exePath)

	// Create temp directory in the SAME directory as executable
	// This ensures os.Rename is atomic (same filesystem)
	tempDir, err := os.MkdirTemp(exeDir, ".sub2api-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Download archive
	archivePath := filepath.Join(tempDir, archive.Name)
	if err := s.githubClient.DownloadFile(ctx, archive.DownloadURL, archivePath, maxDownloadSize); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	if err := s.verifyChecksum(ctx, archivePath, checksum.DownloadURL); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	// Extract binary from archive
	newBinaryPath := filepath.Join(tempDir, "sub2api")
	if err := s.extractBinary(archivePath, newBinaryPath); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	// Set executable permission before replacement
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	if _, err := exec.LookPath(newBinaryPath); err != nil {
		return fmt.Errorf("update directory does not allow execution: %w", err)
	}
	// An interrupted HTTP response can cause the same install to be retried before
	// restarting. Preserve the original backup if this binary is already staged.
	if staged, err := os.Stat(exePath); err == nil {
		if next, err := os.Stat(newBinaryPath); err == nil && staged.Size() == next.Size() {
			currentHash, err := updateFileHash(exePath)
			if err != nil {
				return err
			}
			nextHash, err := updateFileHash(newBinaryPath)
			if err != nil {
				return err
			}
			if currentHash == nextHash {
				return nil
			}
		}
	}
	// Keep the live binary in place until the single final rename. On the first
	// container update the original executable lives on a different filesystem.
	previous := filepath.Join(tempDir, "previous")
	if _, err := os.Lstat(exePath); os.IsNotExist(err) && os.Getenv("SUB2API_UPDATE_DIR") != "" {
		if err := os.Symlink("/app/sub2api", previous); err != nil {
			return fmt.Errorf("backup image binary: %w", err)
		}
	} else if err := os.Link(exePath, previous); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(previous, exePath+".backup"); err != nil {
		return fmt.Errorf("save backup: %w", err)
	}
	if err := os.Rename(newBinaryPath, exePath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

// Rollback restores the backup left by the most recent installation.
func (s *UpdateService) Rollback() error {
	if !s.selfUpdateEnabled() {
		return ErrSelfUpdateUnavailable
	}
	exePath, err := updateTarget()
	if err != nil {
		return err
	}
	return os.Rename(exePath+".backup", exePath)
}

// ListRollbackVersions returns up to maxRollbackVersions release versions that are
// strictly older than the current version (the current version itself is excluded),
// newest first. Draft and prerelease entries are skipped.
func (s *UpdateService) ListRollbackVersions(ctx context.Context) ([]RollbackVersion, error) {
	versions := make([]RollbackVersion, 0, maxRollbackVersions)
	if !s.selfUpdateEnabled() {
		return versions, nil
	}
	releases, err := s.rollbackCandidates(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range releases {
		versions = append(versions, RollbackVersion{Version: r.version, TagName: r.info.TagName, PublishedAt: r.info.PublishedAt, HTMLURL: r.info.HTMLURL})
	}
	return versions, nil
}

func (s *UpdateService) RollbackToVersion(ctx context.Context, version string) error {
	if !s.selfUpdateEnabled() {
		return ErrSelfUpdateUnavailable
	}
	target := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if target == "" {
		return ErrRollbackVersionNotAllowed
	}
	releases, err := s.rollbackCandidates(ctx)
	if err != nil {
		return err
	}
	for _, r := range releases {
		if r.version == target {
			return s.applyReleaseAssets(ctx, r.info)
		}
	}
	return ErrRollbackVersionNotAllowed
}

func (s *UpdateService) verifyChecksum(ctx context.Context, filePath, checksumURL string) error {
	// Download checksums file
	checksumData, err := s.githubClient.FetchChecksumFile(ctx, checksumURL)
	if err != nil {
		return fmt.Errorf("failed to download checksums: %w", err)
	}

	actualHash, err := updateFileHash(filePath)
	if err != nil {
		return err
	}

	// Find expected hash in checksums file
	fileName := filepath.Base(filePath)
	scanner := bufio.NewScanner(strings.NewReader(string(checksumData)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) == 2 && strings.TrimPrefix(parts[1], "*") == fileName {
			if parts[0] == actualHash {
				return nil
			}
			return fmt.Errorf("checksum mismatch: expected %s, got %s", parts[0], actualHash)
		}
	}

	return fmt.Errorf("checksum not found for %s", fileName)
}

func updateFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *UpdateService) extractBinary(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(io.LimitReader(gzr, maxDownloadSize+1))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("binary not found in archive")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || strings.TrimPrefix(hdr.Name, "./") != "sub2api" {
			continue
		}
		if hdr.Size <= 0 || hdr.Size > maxDownloadSize {
			return fmt.Errorf("invalid binary size: %d", hdr.Size)
		}
		out, err := os.Create(destPath)
		if err != nil {
			return err
		}
		_, err = io.CopyN(out, tr, hdr.Size)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
}

func (s *UpdateService) getFromCache(ctx context.Context) (*UpdateInfo, error) {
	data, err := s.cache.GetUpdateInfo(ctx)
	if err != nil {
		return nil, err
	}

	var cached struct {
		ReleaseInfo *ReleaseInfo `json:"release_info"`
		Timestamp   int64        `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(data), &cached); err != nil {
		return nil, err
	}

	if time.Now().Unix()-cached.Timestamp > updateCacheTTL {
		return nil, fmt.Errorf("cache expired")
	}

	info, err := s.updateInfo(cached.ReleaseInfo)
	if err != nil {
		return nil, err
	}
	info.Cached = true
	return info, nil
}

func (s *UpdateService) saveToCache(ctx context.Context, info *UpdateInfo) {
	cacheData := struct {
		ReleaseInfo *ReleaseInfo `json:"release_info"`
		Timestamp   int64        `json:"timestamp"`
	}{
		ReleaseInfo: info.ReleaseInfo,
		Timestamp:   time.Now().Unix(),
	}

	data, _ := json.Marshal(cacheData)
	_ = s.cache.SetUpdateInfo(ctx, string(data), time.Duration(updateCacheTTL)*time.Second)
}
