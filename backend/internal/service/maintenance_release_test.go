//go:build unit

package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type maintenanceAssetClient struct {
	updateServiceGitHubClientStub
	files map[string][]byte
}

func (c *maintenanceAssetClient) FetchChecksumFile(_ context.Context, url string) ([]byte, error) {
	data, ok := c.files[url]
	if !ok {
		return nil, fmt.Errorf("asset missing")
	}
	return data, nil
}
func (c *maintenanceAssetClient) DownloadFile(ctx context.Context, url, dest string, _ int64) error {
	data, err := c.FetchChecksumFile(ctx, url)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0600)
}

func newMaintenanceInstallFixture(t *testing.T) (*UpdateService, *maintenanceAssetClient, string) {
	t.Helper()
	version := "0.2.17-ablett.2"
	binary := []byte("maintenance version two")
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "sub2api", Size: int64(len(binary)), Mode: 0755}))
	_, err := tw.Write(binary)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	manifest := MaintenanceBuildManifest(version, strings.Repeat("a", 40))
	manifest.Archive = fmt.Sprintf("sub2api_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	hash := sha256.Sum256(buffer.Bytes())
	manifest.SHA256 = hex.EncodeToString(hash[:])
	hash = sha256.Sum256(binary)
	manifest.BinarySHA256 = hex.EncodeToString(hash[:])
	m, err := json.Marshal(manifest)
	require.NoError(t, err)
	client := &maintenanceAssetClient{files: map[string][]byte{}}
	client.release = &GitHubRelease{TagName: "v" + version}
	for name, data := range map[string][]byte{manifest.Archive: buffer.Bytes(), "checksums.txt": []byte(manifest.SHA256 + "  " + manifest.Archive + "\n"), fmt.Sprintf("manifest-%s-%s.json", runtime.GOOS, runtime.GOARCH): m} {
		url := "https://github.com/" + MaintenanceRepository + "/releases/download/v" + version + "/" + name
		client.files[url] = data
		client.release.Assets = append(client.release.Assets, GitHubAsset{Name: name, BrowserDownloadURL: url})
	}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.17-ablett.1", "release")
	svc.commit = strings.Repeat("b", 40)
	executable := filepath.Join(t.TempDir(), "sub2api")
	require.NoError(t, os.WriteFile(executable, []byte("maintenance version one"), 0755))
	svc.executablePath = func() (string, error) { return executable, nil }
	return svc, client, executable
}

func TestMaintenanceReleaseAtomicInstallAndBackup(t *testing.T) {
	svc, _, exe := newMaintenanceInstallFixture(t)
	require.NoError(t, svc.PerformUpdate(context.Background()))
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	require.Equal(t, "maintenance version two", string(data))
	require.FileExists(t, exe+".backup.json")
	require.NoError(t, svc.Rollback())
	data, err = os.ReadFile(exe)
	require.NoError(t, err)
	require.Equal(t, "maintenance version one", string(data))
}

func TestMaintenanceReleaseRejectsCorruptionAndOriginalBackup(t *testing.T) {
	svc, client, exe := newMaintenanceInstallFixture(t)
	for url := range client.files {
		if strings.HasSuffix(url, ".tar.gz") {
			client.files[url] = []byte("corrupt")
		}
	}
	require.ErrorContains(t, svc.PerformUpdate(context.Background()), "checksum")
	data, err := os.ReadFile(exe)
	require.NoError(t, err)
	require.Equal(t, "maintenance version one", string(data))
	require.NoError(t, os.WriteFile(exe+".backup", []byte("official"), 0755))
	require.ErrorContains(t, svc.Rollback(), "metadata")
	data, err = os.ReadFile(exe)
	require.NoError(t, err)
	require.Equal(t, "maintenance version one", string(data))
}

func TestMaintenanceReleaseCompatibilityAndSourceCache(t *testing.T) {
	svc, client, _ := newMaintenanceInstallFixture(t)
	svc.databaseState = func(context.Context) (map[string]string, error) {
		return map[string]string{"999_incompatible.sql": "checksum"}, nil
	}
	info, err := svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.Contains(t, info.Warning, "999_incompatible.sql")
	svc.databaseState = nil
	for url, data := range client.files {
		if strings.HasSuffix(url, ".json") {
			var m MaintenanceManifest
			require.NoError(t, json.Unmarshal(data, &m))
			m.Capabilities = nil
			client.files[url], _ = json.Marshal(m)
		}
	}
	info, err = svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.Contains(t, info.Warning, "capability")
	svc.cache = &updateServiceCacheStub{data: fmt.Sprintf(`{"latest":"99.0.0","timestamp":%d,"source":{"repository":"Wei-Shaw/sub2api"}}`, time.Now().Unix())}
	_, err = svc.getFromCache(context.Background())
	require.ErrorContains(t, err, "source")
}

func TestMaintenanceReleaseRevisionComparison(t *testing.T) {
	for _, pair := range [][2]string{{"0.2.17-ablett.1", "0.2.17-ablett.2"}, {"0.2.17-ablett.2", "0.2.17-ablett.10"}, {"0.2.17-ablett.10", "0.2.18-ablett.1"}, {"0.2.17-ablett.2-rc.1", "0.2.17-ablett.2"}} {
		require.Equal(t, -1, compareVersions(pair[0], pair[1]))
		require.Equal(t, 1, compareVersions(pair[1], pair[0]))
	}
	require.True(t, matchesSemverRange("0.2.17-ablett.2", ">=0.2.16 <0.3.0"))
	require.NotEqual(t, normalizeSemver("0.2.17"), normalizeSemver("0.2.17-ablett.2"), "Fork builds cannot be labeled as tested upstream versions")
}
