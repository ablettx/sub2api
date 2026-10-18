package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/migrations"
	"golang.org/x/mod/semver"
)

const (
	MaintenanceRepository      = "ablettx/sub2api"
	MaintenanceChannel         = "stable"
	MaintenanceUpstream        = "Wei-Shaw/sub2api"
	MaintenanceImage           = "ghcr.io/ablettx/sub2api"
	MaintenanceDatabaseVersion = 1
)

type MaintenanceSource struct {
	Repository         string `json:"repository"`
	Channel            string `json:"channel"`
	UpstreamRepository string `json:"upstream_repository"`
	Image              string `json:"image"`
}

func DefaultMaintenanceSource() MaintenanceSource {
	return MaintenanceSource{MaintenanceRepository, MaintenanceChannel, MaintenanceUpstream, MaintenanceImage}
}

type MaintenanceManifest struct {
	SchemaVersion   int    `json:"schema_version"`
	Source          string `json:"source"`
	Channel         string `json:"channel"`
	Version         string `json:"version"`
	UpstreamVersion string `json:"upstream_version"`
	Revision        int    `json:"revision"`
	Commit          string `json:"sha"`
	Target          struct {
		GOOS   string `json:"goos"`
		GOARCH string `json:"goarch"`
	} `json:"target"`
	Archive       string            `json:"archive"`
	SHA256        string            `json:"sha256"`
	BinarySHA256  string            `json:"binary_sha256"`
	Capabilities  map[string]int    `json:"capabilities"`
	DatabaseMin   int               `json:"database_min"`
	DatabaseMax   int               `json:"database_max"`
	RollbackFloor string            `json:"rollback_floor"`
	Migrations    map[string]string `json:"migrations"`
}

var maintenanceVersionPattern = regexp.MustCompile(`^(\d+\.\d+\.\d+)-ablett\.([1-9][0-9]*)(?:-(.+))?$`)
var maintenanceDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var maintenanceCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func parseMaintenanceVersion(version string) (base string, revision int, stable bool) {
	parts := maintenanceVersionPattern.FindStringSubmatch(strings.TrimPrefix(version, "v"))
	if len(parts) != 4 || !semver.IsValid("v"+parts[1]) {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, false
	}
	return parts[1], n, parts[3] == ""
}

func maintenanceVersionCompare(a, b string) int {
	baseA, revA, stableA := parseMaintenanceVersion(a)
	baseB, revB, stableB := parseMaintenanceVersion(b)
	if baseA == "" || baseB == "" {
		return semver.Compare(normalizeSemver(a), normalizeSemver(b))
	}
	if result := semver.Compare("v"+baseA, "v"+baseB); result != 0 {
		return result
	}
	if revA < revB {
		return -1
	}
	if revA > revB {
		return 1
	}
	if stableA && !stableB {
		return 1
	}
	if !stableA && stableB {
		return -1
	}
	return semver.Compare(normalizeSemver(a), normalizeSemver(b))
}

func currentMaintenanceMigrations() map[string]string {
	result := map[string]string{}
	entries, _ := migrations.FS.ReadDir(".")
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := migrations.FS.ReadFile(entry.Name())
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		result[entry.Name()] = hex.EncodeToString(sum[:])
	}
	return result
}

func (s *UpdateService) validateMaintenanceManifest(ctx context.Context, manifest *MaintenanceManifest, version string) error {
	base, revision, stable := parseMaintenanceVersion(version)
	if manifest == nil || !stable || manifest.Version != strings.TrimPrefix(version, "v") || manifest.SchemaVersion != 1 ||
		manifest.Source != s.source.Repository || manifest.Channel != s.source.Channel || manifest.UpstreamVersion != base || manifest.Revision != revision ||
		manifest.Target.GOOS != runtime.GOOS || manifest.Target.GOARCH != runtime.GOARCH ||
		manifest.Capabilities["account_user_quota"] < 1 || manifest.DatabaseMin > MaintenanceDatabaseVersion || manifest.DatabaseMax < MaintenanceDatabaseVersion ||
		!maintenanceCommitPattern.MatchString(manifest.Commit) || !maintenanceDigestPattern.MatchString(manifest.SHA256) || !maintenanceDigestPattern.MatchString(manifest.BinarySHA256) {
		return errors.New("release lacks compatible Fork provenance, platform, quota capability or database metadata")
	}
	_, _, floorStable := parseMaintenanceVersion(manifest.RollbackFloor)
	if !floorStable || maintenanceVersionCompare(version, manifest.RollbackFloor) < 0 ||
		maintenanceVersionCompare(version, s.rollbackFloor) < 0 {
		return errors.New("release is below the compatible rollback floor")
	}
	state := currentMaintenanceMigrations()
	if s.databaseState != nil {
		var err error
		state, err = s.databaseState(ctx)
		if err != nil {
			return fmt.Errorf("read database compatibility: %w", err)
		}
	}
	for name, checksum := range state {
		if manifest.Migrations[name] != checksum {
			return fmt.Errorf("release cannot read applied database migration %s", name)
		}
	}
	return nil
}

func (s *UpdateService) fetchMaintenanceManifest(ctx context.Context, version string, assets []Asset) (*MaintenanceManifest, error) {
	name := fmt.Sprintf("manifest-%s-%s.json", runtime.GOOS, runtime.GOARCH)
	var assetURL string
	for _, asset := range assets {
		if asset.Name == name {
			if assetURL != "" {
				return nil, errors.New("duplicate release manifest")
			}
			assetURL = asset.DownloadURL
		}
	}
	if assetURL == "" {
		return nil, errors.New("compatible Fork release manifest is missing")
	}
	if err := s.validateReleaseAssetURL(assetURL, version, name); err != nil {
		return nil, err
	}
	data, err := s.githubClient.FetchChecksumFile(ctx, assetURL)
	if err != nil {
		return nil, err
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("release manifest exceeds size limit")
	}
	var manifest MaintenanceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := s.validateMaintenanceManifest(ctx, &manifest, version); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (s *UpdateService) validateReleaseAssetURL(raw, version, name string) error {
	if raw != "https://github.com/"+s.source.Repository+"/releases/download/v"+strings.TrimPrefix(version, "v")+"/"+name {
		return errors.New("release asset does not belong to the configured Fork and version")
	}
	return validateDownloadURL(raw)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type maintenanceBackup struct {
	Manifest     MaintenanceManifest `json:"manifest"`
	BinarySHA256 string              `json:"binary_sha256"`
}

// MaintenanceBuildManifest is safe to expose without loading runtime configuration.
// It contains build provenance and public migration hashes, never credentials.
func MaintenanceBuildManifest(version, commit string) MaintenanceManifest {
	base, revision, _ := parseMaintenanceVersion(version)
	manifest := MaintenanceManifest{SchemaVersion: 1, Source: MaintenanceRepository, Channel: MaintenanceChannel,
		Version: strings.TrimPrefix(version, "v"), UpstreamVersion: base, Revision: revision, Commit: commit,
		Capabilities: map[string]int{"account_user_quota": 1}, DatabaseMin: 1, DatabaseMax: 1,
		RollbackFloor: "0.2.17-ablett.1", Migrations: currentMaintenanceMigrations()}
	manifest.Target.GOOS, manifest.Target.GOARCH = runtime.GOOS, runtime.GOARCH
	return manifest
}

func (s *UpdateService) prepareMaintenanceBackup(ctx context.Context, executable, tempDir, backupPath string) error {
	base, revision, stable := parseMaintenanceVersion(s.currentVersion)
	if !stable || !maintenanceCommitPattern.MatchString(s.commit) {
		return errors.New("current build lacks verified maintenance metadata; install the first Fork release explicitly")
	}
	digest, err := fileSHA256(executable)
	if err != nil {
		return err
	}
	if s.runningBinarySHA != "" && digest != s.runningBinarySHA {
		return ErrMaintenanceRestartRequired
	}
	manifest := MaintenanceManifest{SchemaVersion: 1, Source: s.source.Repository, Channel: s.source.Channel, Version: strings.TrimPrefix(s.currentVersion, "v"),
		UpstreamVersion: base, Revision: revision, Commit: s.commit, Archive: "backup", SHA256: digest, BinarySHA256: digest,
		Capabilities: map[string]int{"account_user_quota": 1}, DatabaseMin: 1, DatabaseMax: 1, RollbackFloor: s.rollbackFloor, Migrations: currentMaintenanceMigrations()}
	manifest.Target.GOOS, manifest.Target.GOARCH = runtime.GOOS, runtime.GOARCH
	if err := s.validateMaintenanceManifest(ctx, &manifest, s.currentVersion); err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	prepared := filepath.Join(tempDir, "backup")
	output, err := os.OpenFile(prepared, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, source)
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	data, err := json.Marshal(maintenanceBackup{Manifest: manifest, BinarySHA256: digest})
	if err != nil {
		return err
	}
	metadata := filepath.Join(tempDir, "backup.json")
	if err := os.WriteFile(metadata, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(prepared, backupPath); err != nil {
		return err
	}
	return os.Rename(metadata, backupPath+".json")
}
