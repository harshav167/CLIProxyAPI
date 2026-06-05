package redisqueue

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRetentionSeconds int64 = 60
	maxRetentionSeconds     int64 = 3600
	usageSubscriberBuffer         = 256
	errorSubscriberBuffer         = 256

	usageSupportRefreshPayload = `{"support_refresh":true}`
	usageRefreshPayload        = `{"refresh":true}`
)

type usageSubscriberRegistry struct {
	mu          sync.Mutex
	subscribers map[uint64]chan []byte
	nextID      uint64
}

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

	// Storage layer: pluggable backend (in-memory default, optional Redis/Valkey).
	backendMu      sync.RWMutex
	currentBackend backend = newInMemoryBackend()

	usageSubscribers usageSubscriberRegistry

	// Error-event subscription/notify layer (upstream). Usage refresh uses our
	// registry (usageSubscribers); errorGlobal backs the new error stream.
	errorGlobal queue
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
		clearUsageSubscribers()
		getBackend().Clear()
		errorGlobal.clear()
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
	// Live SUBSCRIBE fanout and poll-based LPOP are alternative transports, so
	// we only short-circuit the backend enqueue when the payload was actually
	// delivered to at least one live subscriber. When there are no subscribers,
	// or every subscriber's buffer was full (and thus dropped), the record
	// would otherwise be lost — fall through and persist it to the backend.
	if publishToUsageSubscribers(payload) {
		return
	}
	getBackend().Enqueue(payload)
}

func SubscribeUsage() (<-chan []byte, func()) {
	subscriber := make(chan []byte, usageSubscriberBuffer)
	// Emit the support-refresh marker immediately so a new subscriber learns the
	// stream supports refresh notifications (matches upstream's contract).
	subscriber <- []byte(usageSupportRefreshPayload)

	usageSubscribers.mu.Lock()
	if usageSubscribers.subscribers == nil {
		usageSubscribers.subscribers = make(map[uint64]chan []byte)
	}
	usageSubscribers.nextID++
	id := usageSubscribers.nextID
	usageSubscribers.subscribers[id] = subscriber
	usageSubscribers.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			unsubscribeUsageSubscriber(id)
		})
	}
	return subscriber, unsubscribe
}

// publishToUsageSubscribers attempts to fan the payload out to every live
// subscriber. It reports whether the payload was delivered to at least one
// subscriber. A subscriber whose buffer is full is dropped (and closed), and
// does NOT count as a delivery — so when every subscriber is full (or there
// are none), this returns false and the caller persists the payload to the
// backend instead of silently losing it.
func publishToUsageSubscribers(payload []byte) bool {
	usageSubscribers.mu.Lock()
	defer usageSubscribers.mu.Unlock()

	if len(usageSubscribers.subscribers) == 0 {
		return false
	}

	delivered := false
	for id, subscriber := range usageSubscribers.subscribers {
		cloned := append([]byte(nil), payload...)
		select {
		case subscriber <- cloned:
			delivered = true
		default:
			delete(usageSubscribers.subscribers, id)
			close(subscriber)
		}
	}
	return delivered
}

func unsubscribeUsageSubscriber(id uint64) {
	usageSubscribers.mu.Lock()
	subscriber, ok := usageSubscribers.subscribers[id]
	if ok {
		delete(usageSubscribers.subscribers, id)
	}
	usageSubscribers.mu.Unlock()

	if ok {
		close(subscriber)
	}
}

func clearUsageSubscribers() {
	usageSubscribers.mu.Lock()
	subscribers := make([]chan []byte, 0, len(usageSubscribers.subscribers))
	for _, subscriber := range usageSubscribers.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	usageSubscribers.subscribers = nil
	usageSubscribers.mu.Unlock()

	for _, subscriber := range subscribers {
		close(subscriber)
	}
}

func EnqueueError(payload []byte) {
	if !Enabled() {
		return
	}
	if len(payload) == 0 {
		return
	}
	errorGlobal.publishToSubscribers(payload)
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

// SubscribeErrors exposes the upstream error-event stream. Usage subscriptions
// use our registry-based SubscribeUsage above; errors use the errorGlobal queue.
func SubscribeErrors() (<-chan []byte, func()) {
	return errorGlobal.subscribe(errorSubscriberBuffer, nil)
}

// NotifyUsageRefresh fans a refresh signal to live usage subscribers via our
// registry (the same mechanism SubscribeUsage registers with).
func NotifyUsageRefresh() {
	publishToUsageSubscribers([]byte(usageRefreshPayload))
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
	errorGlobal.clear()
}

func (q *queue) enqueue(payload []byte) {
	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.pruneLocked(now)
	q.items = append(q.items, queueItem{
		enqueuedAt: now,
		payload:    append([]byte(nil), payload...),
	})
	q.maybeCompactLocked()
}

func (q *queue) publishToSubscribers(payload []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.subscribers) == 0 {
		return false
	}

	for id, subscriber := range q.subscribers {
		cloned := append([]byte(nil), payload...)
		select {
		case subscriber <- cloned:
		default:
			delete(q.subscribers, id)
			close(subscriber)
		}
	}

	return true
}

func (q *queue) subscribe(buffer int, initialPayload []byte) (<-chan []byte, func()) {
	subscriber := make(chan []byte, buffer)
	if len(initialPayload) > 0 {
		subscriber <- append([]byte(nil), initialPayload...)
	}

	q.mu.Lock()
	if q.subscribers == nil {
		q.subscribers = make(map[uint64]chan []byte)
	}
	q.nextSubscriberID++
	id := q.nextSubscriberID
	q.subscribers[id] = subscriber
	q.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			q.unsubscribe(id)
		})
	}
	return subscriber, unsubscribe
}

func (q *queue) unsubscribe(id uint64) {
	q.mu.Lock()
	subscriber, ok := q.subscribers[id]
	if ok {
		delete(q.subscribers, id)
	}
	q.mu.Unlock()

	if ok {
		close(subscriber)
	}
}

func (q *queue) clear() {
	q.mu.Lock()

	subscribers := make([]chan []byte, 0, len(q.subscribers))
	for _, subscriber := range q.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	q.items = nil
	q.head = 0
	q.subscribers = nil
	q.mu.Unlock()

	for _, subscriber := range subscribers {
		close(subscriber)
	}
}

func (q *queue) popOldest(count int) [][]byte {
	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.pruneLocked(now)
	available := len(q.items) - q.head
	if available <= 0 {
		q.items = nil
		q.head = 0
		return nil
	}
	if count > available {
		count = available
	}

	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		item := q.items[q.head+i]
		out = append(out, item.payload)
	}
	q.head += count
	q.maybeCompactLocked()
	return out
}

func (q *queue) pruneLocked(now time.Time) {
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}

	windowSeconds := retentionSeconds.Load()
	if windowSeconds <= 0 {
		windowSeconds = defaultRetentionSeconds
	}
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second)
	for q.head < len(q.items) && q.items[q.head].enqueuedAt.Before(cutoff) {
		q.head++
	}
}

func (q *queue) maybeCompactLocked() {
	if q.head == 0 {
		return
	}
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}
	if q.head < 1024 && q.head*2 < len(q.items) {
		return
	}
	q.items = append([]queueItem(nil), q.items[q.head:]...)
	q.head = 0
}

// queue is the subscription/notify primitive (upstream): a retention-bounded
// buffer with fan-out subscribers. Used by global (usage-refresh) and
// errorGlobal (error events), layered alongside the pluggable storage backend.
type queue struct {
	mu               sync.Mutex
	items            []queueItem
	head             int
	subscribers      map[uint64]chan []byte
	nextSubscriberID uint64
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
