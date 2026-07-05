package helps

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// Connection-pool tuning for upstream provider transports. Go's
// http.DefaultTransport caps MaxIdleConnsPerHost at 2, which throttles idle
// connection reuse when the proxy funnels many concurrent requests to a single
// upstream host (every provider is one host) — forcing repeated TCP+TLS
// handshakes under load. These limits keep a warm pool per upstream host.
const (
	proxyMaxIdleConns        = 1000
	proxyMaxIdleConnsPerHost = 100
	proxyIdleConnTimeout     = 90 * time.Second
)

// transportCache memoises one *http.Transport per resolved proxy setting so
// that connection pools are shared across requests. Without this, the proxy
// path (buildProxyTransport) allocated a fresh transport — and therefore an
// empty connection pool — on every request, so no upstream connection was ever
// reused (verified: 10 dials for 10 sequential requests). The cache key is the
// resolved proxy string ("" = no proxy, "direct", or the proxy URL), which is
// exactly what determines transport behaviour, so distinct settings never share
// a transport.
var (
	transportCacheMu sync.RWMutex
	transportCache   = map[string]*http.Transport{}
)

// tuneTransportPool applies the shared idle-connection limits to a transport.
func tuneTransportPool(t *http.Transport) *http.Transport {
	if t == nil {
		return nil
	}
	t.MaxIdleConns = proxyMaxIdleConns
	t.MaxIdleConnsPerHost = proxyMaxIdleConnsPerHost
	t.IdleConnTimeout = proxyIdleConnTimeout
	return t
}

// cachedProxyTransport returns a shared, pool-tuned transport for the resolved
// proxy string, building it once on first use. Returns nil when the setting
// yields no explicit transport (ModeInherit), letting callers fall back to the
// context RoundTripper or http.DefaultTransport.
func cachedProxyTransport(proxyURL string) *http.Transport {
	transportCacheMu.RLock()
	if t, ok := transportCache[proxyURL]; ok {
		transportCacheMu.RUnlock()
		return t
	}
	transportCacheMu.RUnlock()

	transportCacheMu.Lock()
	defer transportCacheMu.Unlock()
	if t, ok := transportCache[proxyURL]; ok {
		return t
	}
	transport := buildProxyTransport(proxyURL)
	if transport == nil && strings.TrimSpace(proxyURL) == "" {
		// ModeInherit with no proxy: build a tuned clone of the default
		// transport so the no-proxy path still gets a shared, higher-limit pool
		// instead of falling back to http.DefaultTransport (2 idle conns/host).
		transport = proxyutil.NewDirectTransport()
		transport.Proxy = http.ProxyFromEnvironment
	}
	transport = tuneTransportPool(transport)
	// Cache even a nil result so we don't rebuild-and-discard on every call for
	// settings that legitimately produce no transport (e.g. an invalid proxy
	// URL that should fall through to the context RoundTripper).
	transportCache[proxyURL] = transport
	return transport
}

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// If we have a proxy URL configured, use the shared (pool-tuned, cached)
	// transport for that setting so connections are reused across requests.
	if proxyURL != "" {
		if transport := cachedProxyTransport(proxyURL); transport != nil {
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor).
	// This is per-auth (e.g. the uTLS client) and manages its own connection
	// pool, so we do not override it.
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
		return httpClient
	}

	// Priority 4: No proxy and no context RoundTripper (the common openai-compat
	// path). Leaving Transport nil would fall back to http.DefaultTransport,
	// whose MaxIdleConnsPerHost is only 2 — a bottleneck when many concurrent
	// requests target one upstream host. Use a shared pool-tuned transport
	// (cached under the "" key) instead.
	httpClient.Transport = cachedProxyTransport("")

	return httpClient
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
