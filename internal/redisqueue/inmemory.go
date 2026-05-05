package redisqueue

import (
	"sync"
	"time"
)

type queueItem struct {
	enqueuedAt time.Time
	payload    []byte
}

// inMemoryBackend is the default backend; it keeps recent payloads in a
// process-local ring buffer and prunes them based on the configured retention
// window.
type inMemoryBackend struct {
	mu    sync.Mutex
	items []queueItem
	head  int
}

func newInMemoryBackend() *inMemoryBackend {
	return &inMemoryBackend{}
}

func (q *inMemoryBackend) Enqueue(payload []byte) {
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

func (q *inMemoryBackend) PopOldest(count int) [][]byte {
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
		out = append(out, q.items[q.head+i].payload)
	}
	q.head += count
	q.maybeCompactLocked()
	return out
}

func (q *inMemoryBackend) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = nil
	q.head = 0
}

func (q *inMemoryBackend) Close() error { return nil }

func (q *inMemoryBackend) pruneLocked(now time.Time) {
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}
	windowSeconds := currentRetentionSeconds()
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second)
	for q.head < len(q.items) && q.items[q.head].enqueuedAt.Before(cutoff) {
		q.head++
	}
}

func (q *inMemoryBackend) maybeCompactLocked() {
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
