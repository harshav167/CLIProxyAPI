package pluginstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
	log "github.com/sirupsen/logrus"
)

const userAgent = "CLIProxyAPI"
const maxPluginStoreRedirects = 10

var defaultPluginStoreHTTPClient HTTPDoer = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   5,
	},
}

// DefaultHTTPClient returns the package-level default HTTP client.
func DefaultHTTPClient() HTTPDoer {
	return defaultPluginStoreHTTPClient
}

type HTTPDoer = httpfetch.Doer

type Client struct {
	HTTPClient  HTTPDoer
	RegistryURL string
	UserAgent   string
	Auth        []AuthConfig
}

type Release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

type ReleaseAsset struct {
	APIURL             string `json:"url"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (c Client) FetchRegistry(ctx context.Context) (Registry, error) {
	registryURL := strings.TrimSpace(c.RegistryURL)
	if registryURL == "" {
		registryURL = DefaultRegistryURL
	}
	data, errDownload := c.get(ctx, registryURL, "application/json", RequestKindRegistry, 0)
	if errDownload != nil {
		return Registry{}, errDownload
	}
	registry, errParse := ParseRegistry(data)
	if errParse != nil {
		return Registry{}, errParse
	}
	return registry, nil
}

// FetchLatestRelease returns the latest published release of the plugin's
// GitHub repository, mirroring the WebUI panel update check.
func (c Client) FetchLatestRelease(ctx context.Context, plugin Plugin) (Release, error) {
	return c.fetchRelease(ctx, plugin, "releases/latest")
}

// FetchReleaseByTag returns a published release by its exact GitHub tag.
func (c Client) FetchReleaseByTag(ctx context.Context, plugin Plugin, tag string) (Release, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return Release{}, fmt.Errorf("release tag is required")
	}
	return c.fetchRelease(ctx, plugin, "releases/tags/"+url.PathEscape(tag))
}

func (c Client) fetchRelease(ctx context.Context, plugin Plugin, suffix string) (Release, error) {
	owner, repo, err := GitHubRepositoryParts(plugin.Repository)
	if err != nil {
		return Release{}, err
	}
	releaseURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/%s", url.PathEscape(owner), url.PathEscape(repo), suffix)
	data, err := c.get(ctx, releaseURL, "application/vnd.github+json", RequestKindMetadata, 0)
	if err != nil {
		return Release{}, err
	}
	var release Release
	if err := json.Unmarshal(data, &release); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	return release, nil
}

// ReleaseVersion derives the plugin version from the release tag, stripping a
// leading "v"/"V" and validating the result.
func ReleaseVersion(release Release) (string, error) {
	version := normalizeVersion(release.TagName)
	if !validPluginVersion(version) {
		return "", fmt.Errorf("invalid release tag %q", release.TagName)
	}
	return version, nil
}

func (c Client) DownloadAsset(ctx context.Context, asset ReleaseAsset) ([]byte, error) {
	downloadURL := asset.BrowserDownloadURL
	if downloadURL == "" || c.releaseAssetAPIAuthenticated(asset.APIURL) {
		downloadURL = asset.APIURL
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("asset %q missing download url", asset.Name)
	}
	return c.get(ctx, downloadURL, "application/octet-stream", RequestKindArtifact, 0)
}

func (c Client) releaseAssetAPIAuthenticated(apiURL string) bool {
	return AuthConfigured(c.Auth, apiURL, RequestKindArtifact)
}

func (c Client) get(ctx context.Context, requestURL string, accept string, kind string, maxSize int64) ([]byte, error) {
	currentURL := strings.TrimSpace(requestURL)
	for redirects := 0; ; redirects++ {
		if errURL := validatePluginStoreRequestURL(c.Auth, currentURL, kind); errURL != nil {
			return nil, errURL
		}
		headers := http.Header{
			"Accept":     []string{accept},
			"User-Agent": []string{c.userAgent()},
		}
		if errAuth := applyPluginStoreAuth(headers, c.Auth, currentURL, kind); errAuth != nil {
			return nil, errAuth
		}
		resp, errDo := pluginStoreGetNoRedirect(ctx, c.httpClient(), currentURL, headers)
		if errDo != nil {
			return nil, errDo
		}
		if pluginStoreRedirectStatus(resp.StatusCode) {
			nextURL, errRedirect := pluginStoreRedirectURL(resp, currentURL)
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close plugin store redirect body")
			}
			if errRedirect != nil {
				return nil, errRedirect
			}
			if redirects >= maxPluginStoreRedirects {
				return nil, fmt.Errorf("stopped after %d redirects", maxPluginStoreRedirects)
			}
			currentURL = nextURL
			continue
		}
		return readPluginStoreResponse(resp, maxSize)
	}
}

func (c Client) httpClient() HTTPDoer {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultPluginStoreHTTPClient
}

func (c Client) userAgent() string {
	if v := strings.TrimSpace(c.UserAgent); v != "" {
		return v
	}
	return userAgent
}

func pluginStoreGetNoRedirect(ctx context.Context, client HTTPDoer, requestURL string, headers http.Header) (*http.Response, error) {
	if client == nil {
		client = defaultPluginStoreHTTPClient
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create request: %w", errRequest)
	}
	req.Header = headers.Clone()
	resp, errDo := pluginStoreNoRedirectClient(client).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request failed: %w", errDo)
	}
	return resp, nil
}

func pluginStoreNoRedirectClient(client HTTPDoer) HTTPDoer {
	httpClient, ok := client.(*http.Client)
	if !ok {
		return client
	}
	clone := *httpClient
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func pluginStoreRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func pluginStoreRedirectURL(resp *http.Response, requestURL string) (string, error) {
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return "", fmt.Errorf("redirect missing Location header")
	}
	base, errBase := url.Parse(requestURL)
	if errBase != nil {
		return "", fmt.Errorf("parse redirect base: %w", errBase)
	}
	next, errNext := base.Parse(location)
	if errNext != nil {
		return "", fmt.Errorf("parse redirect location: %w", errNext)
	}
	if next.Scheme == "" || next.Host == "" {
		return "", fmt.Errorf("redirect location is not absolute")
	}
	return next.String(), nil
}

func readPluginStoreResponse(resp *http.Response, maxSize int64) ([]byte, error) {
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close plugin store response body")
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	reader := io.Reader(resp.Body)
	if maxSize > 0 {
		reader = io.LimitReader(resp.Body, maxSize+1)
	}
	data, errRead := io.ReadAll(reader)
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	if maxSize > 0 && int64(len(data)) > maxSize {
		return nil, fmt.Errorf("response exceeds maximum allowed size of %d bytes", maxSize)
	}
	return data, nil
}

func SelectReleaseAssets(release Release, id, version, goos, goarch string) (ReleaseAsset, ReleaseAsset, error) {
	archiveName := ArchiveName(id, version, goos, goarch)
	var archiveAsset ReleaseAsset
	var checksumAsset ReleaseAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case archiveName:
			archiveAsset = asset
		case "checksums.txt":
			checksumAsset = asset
		}
	}
	if archiveAsset.Name == "" {
		return ReleaseAsset{}, ReleaseAsset{}, fmt.Errorf("release asset %s not found", archiveName)
	}
	if checksumAsset.Name == "" {
		return ReleaseAsset{}, ReleaseAsset{}, fmt.Errorf("release asset checksums.txt not found")
	}
	return archiveAsset, checksumAsset, nil
}

func ArchiveName(id, version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", id, version, goos, goarch)
}
