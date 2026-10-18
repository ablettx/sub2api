//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	addMaintenanceTestManifest(s.release)
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	for _, r := range s.recentReleases {
		addMaintenanceTestManifest(r)
	}
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(_ context.Context, raw string) ([]byte, error) {
	parts := strings.Split(raw, "/")
	version := strings.TrimPrefix(parts[len(parts)-2], "v")
	base, revision, _ := parseMaintenanceVersion(version)
	manifest := MaintenanceManifest{SchemaVersion: 1, Source: MaintenanceRepository, Channel: MaintenanceChannel, Version: version, UpstreamVersion: base, Revision: revision, Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("a", 64), BinarySHA256: strings.Repeat("b", 64), Capabilities: map[string]int{"account_user_quota": 1}, DatabaseMin: 1, DatabaseMax: 1, RollbackFloor: "0.2.17-ablett.1", Migrations: currentMaintenanceMigrations()}
	manifest.Target.GOOS, manifest.Target.GOARCH = runtime.GOOS, runtime.GOARCH
	return json.Marshal(manifest)
}
func addMaintenanceTestManifest(r *GitHubRelease) {
	if r == nil || len(r.Assets) > 0 {
		return
	}
	name := fmt.Sprintf("manifest-%s-%s.json", runtime.GOOS, runtime.GOARCH)
	r.Assets = []GitHubAsset{{Name: name, BrowserDownloadURL: "https://github.com/" + MaintenanceRepository + "/releases/download/" + r.TagName + "/" + name}}
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.2.132-ablett.1",
				Name:    "v0.2.132-ablett.1",
			},
		},
		"0.2.132-ablett.1",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.2.148-ablett.1", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.2.147-ablett.1", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.2.146-ablett.1-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.2.146-ablett.1", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.2.145-ablett.1", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.2.144-ablett.1", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.2.144-ablett.1", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.2.143-ablett.1", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.2.142-ablett.1", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.2.147-ablett.1", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.2.146-ablett.1", versions[0].Version)
	require.Equal(t, "0.2.144-ablett.1", versions[1].Version)
	require.Equal(t, "0.2.143-ablett.1", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.2.144-ablett.1"},
		{TagName: "v0.2.146-ablett.1"},
		{TagName: "v0.2.145-ablett.1"},
	}
	svc := newRollbackTestService("0.2.147-ablett.1", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.2.146-ablett.1", versions[0].Version)
	require.Equal(t, "0.2.145-ablett.1", versions[1].Version)
	require.Equal(t, "0.2.144-ablett.1", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.2.147-ablett.1"},
		{TagName: "v0.2.148-ablett.1"},
	}
	svc := newRollbackTestService("0.2.147-ablett.1", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.2.147-ablett.1",
		"release",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.2.148-ablett.1"},
		{TagName: "v0.2.147-ablett.1"},
		{TagName: "v0.2.146-ablett.1"},
		{TagName: "v0.2.145-ablett.1"},
		{TagName: "v0.2.144-ablett.1"},
		{TagName: "v0.2.143-ablett.1"},
		{TagName: "v0.2.142-ablett.1"},
	}
	svc := newRollbackTestService("0.2.147-ablett.1", releases)

	for _, target := range []string{
		"",                  // empty
		"0.2.147-ablett.1",  // current version
		"v0.2.147-ablett.1", // current version with prefix
		"0.2.148-ablett.1",  // newer than current
		"0.2.142-ablett.1",  // older than the 3 most recent
		"9.9.9",             // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.2.147-ablett.1"},
		{TagName: "v0.2.146-ablett.1"},
	}
	svc := newRollbackTestService("0.2.147-ablett.1", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.2.146-ablett.1")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}
