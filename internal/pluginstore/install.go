package pluginstore

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"
)

type InstallOptions struct {
	PluginsDir string
	GOOS       string
	GOARCH     string
	// PluginLoaded reports whether the plugin's dynamic library is currently
	// loaded by the running host. Windows installs are rejected only when they
	// would overwrite an existing target file while it returns true.
	PluginLoaded func() bool
	// BeforeWrite runs after the archive has been downloaded and verified, but
	// before an existing target plugin file is replaced.
	BeforeWrite func() error
}

// ErrLoadedPluginLocked is returned when an install would overwrite a plugin
// library that is loaded by the running process on Windows.
var ErrLoadedPluginLocked = errors.New("loaded plugin library cannot be overwritten while the server is running")

type InstallResult struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	ReleaseTag  string `json:"release_tag,omitempty"`
	InstallType string `json:"install_type,omitempty"`
	Path        string `json:"path"`
	Overwritten bool   `json:"overwritten"`
	Skipped     bool   `json:"skipped"`
}

func (c Client) Install(ctx context.Context, plugin Plugin, options InstallOptions) (InstallResult, error) {
	if err := ValidatePlugin(plugin); err != nil {
		return InstallResult{}, err
	}
	options = normalizeInstallOptions(options)
	if PluginInstallType(plugin) == InstallTypeDirect {
		plugin.Version = normalizeVersion(plugin.Version)
		return c.InstallDirect(ctx, plugin, plugin.Install, options)
	}
	release, err := c.FetchLatestRelease(ctx, plugin)
	if err != nil {
		return InstallResult{}, err
	}
	latestVersion, err := ReleaseVersion(release)
	if err != nil {
		return InstallResult{}, err
	}
	plugin.Version = latestVersion
	return c.installRelease(ctx, plugin, release, latestVersion, options)
}

func (c Client) InstallManifest(ctx context.Context, manifest Manifest, options InstallOptions) (InstallResult, error) {
	if err := manifest.Validate(); err != nil {
		return InstallResult{}, err
	}
	options = normalizeInstallOptions(options)
	switch manifest.InstallType() {
	case InstallTypeDirect:
		plugin, err := c.directPluginFromManifest(ctx, manifest)
		if err != nil {
			return InstallResult{}, err
		}
		return c.InstallDirect(ctx, plugin, plugin.Install, options)
	case InstallTypeGitHubRelease:
		return c.InstallVersion(ctx, manifest.Plugin(), manifest.ReleaseTag, manifest.Version, options)
	default:
		return InstallResult{}, fmt.Errorf("unsupported install type %q", manifest.Install.Type)
	}
}

// InstallVersion installs a plugin artifact from a fixed release tag/version.
func (c Client) InstallVersion(ctx context.Context, plugin Plugin, releaseTag string, version string, options InstallOptions) (InstallResult, error) {
	if err := ValidatePlugin(plugin); err != nil {
		return InstallResult{}, err
	}
	options = normalizeInstallOptions(options)
	version = normalizeVersion(version)
	if !validPluginVersion(version) {
		return InstallResult{}, fmt.Errorf("invalid plugin version %q", version)
	}
	releaseTag = strings.TrimSpace(releaseTag)
	if releaseTag == "" {
		releaseTag = version
	}
	release, err := c.FetchReleaseByTag(ctx, plugin, releaseTag)
	if err != nil {
		return InstallResult{}, err
	}
	releaseVersion, err := ReleaseVersion(release)
	if err != nil {
		return InstallResult{}, err
	}
	if releaseVersion != version {
		return InstallResult{}, fmt.Errorf("release tag %q resolved version %q, want %q", releaseTag, releaseVersion, version)
	}
	plugin.Version = version
	return c.installRelease(ctx, plugin, release, version, options)
}

func (c Client) installRelease(ctx context.Context, plugin Plugin, release Release, version string, options InstallOptions) (InstallResult, error) {
	archiveAsset, checksumAsset, err := SelectReleaseAssets(release, plugin.ID, plugin.Version, options.GOOS, options.GOARCH)
	if err != nil {
		return InstallResult{}, err
	}
	archiveData, err := c.DownloadAsset(ctx, archiveAsset)
	if err != nil {
		return InstallResult{}, fmt.Errorf("download %s: %w", archiveAsset.Name, err)
	}
	checksumData, err := c.DownloadAsset(ctx, checksumAsset)
	if err != nil {
		return InstallResult{}, fmt.Errorf("download checksums.txt: %w", err)
	}
	checksums, err := ParseChecksums(checksumData)
	if err != nil {
		return InstallResult{}, err
	}
	if err := VerifyChecksum(archiveAsset.Name, archiveData, checksums); err != nil {
		return InstallResult{}, err
	}
	plugin.Version = version
	result, err := InstallArchive(archiveData, plugin, options)
	if err != nil {
		return InstallResult{}, err
	}
	result.InstallType = InstallTypeGitHubRelease
	result.ReleaseTag = strings.TrimSpace(release.TagName)
	return result, nil
}

func (c Client) InstallDirect(ctx context.Context, plugin Plugin, plan InstallPlan, options InstallOptions) (InstallResult, error) {
	plugin.ID = strings.TrimSpace(plugin.ID)
	plugin.Version = normalizeVersion(plugin.Version)
	if !validPluginID(plugin.ID) {
		return InstallResult{}, fmt.Errorf("invalid plugin id %q", plugin.ID)
	}
	if !validPluginVersion(plugin.Version) {
		return InstallResult{}, fmt.Errorf("invalid plugin version %q", plugin.Version)
	}
	plan = NormalizeInstallPlan(plan)
	plan.Type = InstallTypeDirect
	if err := ValidateInstallPlan(plan); err != nil {
		return InstallResult{}, err
	}
	options = normalizeInstallOptions(options)
	artifact, err := SelectArtifact(plan, options.GOOS, options.GOARCH)
	if err != nil {
		return InstallResult{}, err
	}
	archiveData, err := c.DownloadArtifact(ctx, artifact)
	if err != nil {
		return InstallResult{}, fmt.Errorf("download artifact: %w", err)
	}
	if err := VerifyArtifactChecksum(artifact, archiveData); err != nil {
		return InstallResult{}, err
	}
	result, err := InstallArchive(archiveData, plugin, options)
	if err != nil {
		return InstallResult{}, err
	}
	result.InstallType = InstallTypeDirect
	return result, nil
}

func (c Client) directPluginFromManifest(ctx context.Context, manifest Manifest) (Plugin, error) {
	plugin := manifest.Plugin()
	plugin.Version = normalizeVersion(manifest.Version)
	plugin.Install = NormalizeInstallPlan(plugin.Install)
	plugin.Install.Type = InstallTypeDirect
	if len(plugin.Install.Artifacts) > 0 {
		return plugin, nil
	}
	sourceURL := strings.TrimSpace(manifest.SourceURL)
	if sourceURL == "" {
		sourceURL = strings.TrimSpace(c.RegistryURL)
	}
	if sourceURL == "" {
		return Plugin{}, fmt.Errorf("direct install manifest missing source-url")
	}
	sourceClient := c
	sourceClient.RegistryURL = sourceURL
	registry, err := sourceClient.FetchRegistry(ctx)
	if err != nil {
		return Plugin{}, fmt.Errorf("fetch direct install source: %w", err)
	}
	resolved, ok := registry.PluginByID(manifest.ID)
	if !ok {
		return Plugin{}, fmt.Errorf("direct install plugin %q not found in source", strings.TrimSpace(manifest.ID))
	}
	if PluginInstallType(resolved) != InstallTypeDirect {
		return Plugin{}, fmt.Errorf("direct install plugin %q resolved as %q", strings.TrimSpace(manifest.ID), PluginInstallType(resolved))
	}
	return directPluginVersion(resolved, manifest.ID, manifest.Version)
}

func directPluginVersion(plugin Plugin, id string, version string) (Plugin, error) {
	id = strings.TrimSpace(id)
	version = normalizeVersion(version)
	if normalizeVersion(plugin.Version) == version {
		plugin.Version = version
		plugin.Install = NormalizeInstallPlan(plugin.Install)
		plugin.Install.Type = InstallTypeDirect
		if err := ValidateInstallPlan(plugin.Install); err != nil {
			return Plugin{}, fmt.Errorf("direct install plugin %q version %q: %w", id, version, err)
		}
		return plugin, nil
	}
	for _, candidate := range plugin.Versions {
		if normalizeVersion(candidate.Version) != version {
			continue
		}
		plugin.Version = version
		plugin.Install = NormalizeInstallPlan(candidate.Install)
		if plugin.Install.Type == "" {
			plugin.Install.Type = InstallTypeDirect
		}
		if plugin.Install.Type != InstallTypeDirect {
			return Plugin{}, fmt.Errorf("direct install plugin %q version %q resolved as %q", id, version, plugin.Install.Type)
		}
		if err := ValidateInstallPlan(plugin.Install); err != nil {
			return Plugin{}, fmt.Errorf("direct install plugin %q version %q: %w", id, version, err)
		}
		return plugin, nil
	}
	return Plugin{}, fmt.Errorf("direct install plugin %q version %q not found in source", id, version)
}

func InstallArchive(archiveData []byte, plugin Plugin, options InstallOptions) (InstallResult, error) {
	options = normalizeInstallOptions(options)
	id := strings.TrimSpace(plugin.ID)
	if !validPluginID(id) {
		return InstallResult{}, fmt.Errorf("invalid plugin id %q", plugin.ID)
	}
	version := normalizeVersion(plugin.Version)
	if !validPluginVersion(version) {
		return InstallResult{}, fmt.Errorf("invalid plugin version %q", plugin.Version)
	}
	plugin.Version = version
	reader, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if err != nil {
		return InstallResult{}, fmt.Errorf("open zip: %w", err)
	}

	libraryData, mode, err := readTargetLibrary(reader, id, version, options.GOOS)
	if err != nil {
		return InstallResult{}, err
	}

	targetPath, err := installTargetPath(options, id, version)
	if err != nil {
		return InstallResult{}, err
	}
	overwritten := false
	if _, err := os.Stat(targetPath); err == nil {
		overwritten = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallResult{}, fmt.Errorf("stat target plugin: %w", err)
	}
	if overwritten {
		existingData, err := os.ReadFile(targetPath)
		if err != nil {
			return InstallResult{}, fmt.Errorf("read target plugin: %w", err)
		}
		if bytes.Equal(existingData, libraryData) {
			return InstallResult{
				ID:          id,
				Version:     plugin.Version,
				Path:        targetPath,
				Overwritten: true,
				Skipped:     true,
			}, nil
		}
	}
	if overwritten && options.BeforeWrite != nil {
		if err := options.BeforeWrite(); err != nil {
			return InstallResult{}, fmt.Errorf("prepare plugin write: %w", err)
		}
	}
	if overwritten && loadedPluginInstallBlocked(options) {
		return InstallResult{}, ErrLoadedPluginLocked
	}
	if err := writeFileAtomic(targetPath, libraryData, mode); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{
		ID:          id,
		Version:     plugin.Version,
		Path:        targetPath,
		Overwritten: overwritten,
	}, nil
}

func installTargetPath(options InstallOptions, id string, version string) (string, error) {
	version = normalizeVersion(version)
	if !validPluginVersion(version) {
		return "", fmt.Errorf("invalid plugin version %q", version)
	}
	return filepath.Join(options.PluginsDir, options.GOOS, options.GOARCH, versionedPluginFileName(id, version, options.GOOS)), nil
}

func readTargetLibrary(reader *zip.Reader, id string, version string, goos string) ([]byte, os.FileMode, error) {
	targetName := strings.TrimSpace(id) + pluginExtension(goos)
	versionedTargetName := versionedPluginFileName(id, version, goos)
	var target *zip.File
	for _, file := range reader.File {
		cleanedName, err := cleanZipName(file.Name)
		if err != nil {
			return nil, 0, err
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !regularZipFile(file) {
			return nil, 0, fmt.Errorf("zip entry %s is not a regular file", file.Name)
		}
		if !hasDynamicLibraryExtension(cleanedName) {
			continue
		}
		if cleanedName != targetName && cleanedName != versionedTargetName {
			if path.Base(cleanedName) == targetName || path.Base(cleanedName) == versionedTargetName {
				return nil, 0, fmt.Errorf("target dynamic library must be at zip root")
			}
			return nil, 0, fmt.Errorf("dynamic library filename must be %s or %s", targetName, versionedTargetName)
		}
		if target != nil {
			return nil, 0, fmt.Errorf("zip contains multiple target dynamic libraries")
		}
		target = file
	}
	if target == nil {
		return nil, 0, fmt.Errorf("zip does not contain %s", targetName)
	}

	handle, err := target.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", targetName, err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			log.WithError(err).Debug("failed to close plugin archive entry")
		}
	}()
	data, err := io.ReadAll(handle)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", targetName, err)
	}
	mode := target.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	return data, mode, nil
}

func versionedPluginFileName(id string, version string, goos string) string {
	return strings.TrimSpace(id) + "-v" + normalizeVersion(version) + pluginExtension(goos)
}

func cleanZipName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("zip entry has empty name")
	}
	if strings.Contains(name, `\`) {
		return "", fmt.Errorf("zip entry %s uses backslash path separators", name)
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("zip entry %s is absolute", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("zip entry %s escapes archive root", name)
	}
	return cleaned, nil
}

func regularZipFile(file *zip.File) bool {
	mode := file.FileInfo().Mode()
	return mode.IsRegular() || mode.Type() == 0
}

func hasDynamicLibraryExtension(name string) bool {
	lowerName := strings.ToLower(name)
	return strings.HasSuffix(lowerName, ".dylib") || strings.HasSuffix(lowerName, ".so") || strings.HasSuffix(lowerName, ".dll")
}

func pluginExtension(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin", "mac", "macos", "osx":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

func writeFileAtomic(targetPath string, data []byte, mode os.FileMode) error {
	targetDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create plugin directory: %w", err)
	}

	temp, err := os.CreateTemp(targetDir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp plugin file: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	closed := false
	defer func() {
		if !closed {
			if err := temp.Close(); err != nil {
				log.WithError(err).Debug("failed to close temp plugin file")
			}
		}
		if removeTemp {
			if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.WithError(err).Debug("failed to remove temp plugin file")
			}
		}
	}()

	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp plugin file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temp plugin file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temp plugin file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp plugin file: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, targetPath); err != nil {
		if runtime.GOOS == "windows" {
			if err := os.Remove(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove old plugin file: %w", err)
			}
			if err := os.Rename(tempPath, targetPath); err == nil {
				removeTemp = false
				return nil
			}
			return fmt.Errorf("install plugin file: %w", err)
		}
		return fmt.Errorf("install plugin file: %w", err)
	}
	removeTemp = false
	return nil
}

func loadedPluginInstallBlocked(options InstallOptions) bool {
	return options.PluginLoaded != nil && strings.EqualFold(options.GOOS, "windows") && options.PluginLoaded()
}

func normalizeInstallOptions(options InstallOptions) InstallOptions {
	options.PluginsDir = strings.TrimSpace(options.PluginsDir)
	if options.PluginsDir == "" {
		options.PluginsDir = "plugins"
	}
	options.GOOS = strings.TrimSpace(options.GOOS)
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	options.GOARCH = strings.TrimSpace(options.GOARCH)
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	options.GOOS = normalizeGOOS(options.GOOS)
	options.GOARCH = normalizeGOARCH(options.GOARCH)
	return options
}
