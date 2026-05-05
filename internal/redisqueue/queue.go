package redisqueue

import (
	"sync"
	"sync/atomic"
)

const (
	defaultRetentionSeconds int64 = 60
	maxRetentionSeconds     int64 = 3600
	usageSubscriberBuffer         = 256
)

// backend abstracts the storage used to buffer usage payloads. The default
// implementation keeps records in-memory; an external Redis/Valkey backend
// can be swapped in via Configure.
type backend interface {
	Enqueue(payload []byte)
	PopOldest(count int) [][]byte
	Clear()
	Close() error
}

var (
	enabled          atomic.Bool
	retentionSeconds atomic.Int64

	backendMu      sync.RWMutex
	currentBackend backend = newInMemoryBackend()
)

func init() {
	retentionSeconds.Store(defaultRetentionSeconds)
}

// SetEnabled toggles the public Enqueue/PopOldest API. When toggled off the
// active backend is cleared. This mirrors the legacy behavior where the queue
// is wired to the management-route lifecycle.
func SetEnabled(value bool) {
	enabled.Store(value)
	if !value {
		getBackend().Clear()
	}
}

func Enabled() bool {
	return enabled.Load()
}

// SetRetentionSeconds clamps the configured retention window. The window
// applies to whichever backend is active.
func SetRetentionSeconds(value int) {
	normalized := int64(value)
	if normalized <= 0 {
		normalized = defaultRetentionSeconds
	} else if normalized > maxRetentionSeconds {
		normalized = maxRetentionSeconds
	}
	retentionSeconds.Store(normalized)
}

func currentRetentionSeconds() int64 {
	v := retentionSeconds.Load()
	if v <= 0 {
		return defaultRetentionSeconds
	}
	return v
}

func Enqueue(payload []byte) {
	if !Enabled() {
		return
	}
	if len(payload) == 0 {
		return
	}
	getBackend().Enqueue(payload)
}

func PopOldest(count int) [][]byte {
	if !Enabled() {
		return nil
	}
	if count <= 0 {
		return nil
	}
	return getBackend().PopOldest(count)
}

func getBackend() backend {
	backendMu.RLock()
	defer backendMu.RUnlock()
	return currentBackend
}

func setBackend(b backend) backend {
	backendMu.Lock()
	prev := currentBackend
	currentBackend = b
	backendMu.Unlock()
	return prev
}

// CurrentBackendName returns a short identifier for the active backend.
// Intended for diagnostics and tests.
func CurrentBackendName() string {
	switch getBackend().(type) {
	case *redisBackend:
		return "redis"
	default:
		return "in-memory"
	}
}

// ResetBackendForTesting forces the in-memory backend. Test-only helper used
// to keep package-level state stable across tests that may have configured an
// external backend.
func ResetBackendForTesting() {
	prev := setBackend(newInMemoryBackend())
	if prev != nil {
		_ = prev.Close()
	}
}

// Shutdown closes any resources held by the active backend. Safe to call
// multiple times.
func Shutdown() error {
	prev := setBackend(newInMemoryBackend())
	if prev == nil {
		return nil
	}
	return prev.Close()
}
