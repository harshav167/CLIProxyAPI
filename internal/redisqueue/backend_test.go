package redisqueue

import (
	"testing"
	"time"
)

func TestDefaultBackendIsInMemory(t *testing.T) {
	ResetBackendForTesting()
	if name := CurrentBackendName(); name != "in-memory" {
		t.Fatalf("default backend = %q, want %q", name, "in-memory")
	}
}

func TestConfigureRedisBackendRequiresAddress(t *testing.T) {
	ResetBackendForTesting()
	if err := ConfigureRedisBackend(RedisBackendConfig{}); err == nil {
		t.Fatalf("ConfigureRedisBackend with empty address should fail")
	}
	if name := CurrentBackendName(); name != "in-memory" {
		t.Fatalf("after failed configure, backend = %q, want %q", name, "in-memory")
	}
}

func TestConfigureRedisBackendUnreachableKeepsInMemory(t *testing.T) {
	ResetBackendForTesting()
	// 127.0.0.1:1 is reserved/unused; dial should fail fast.
	err := ConfigureRedisBackend(RedisBackendConfig{Address: "127.0.0.1:1"})
	if err == nil {
		t.Fatalf("expected ConfigureRedisBackend to fail dialing 127.0.0.1:1")
	}
	if name := CurrentBackendName(); name != "in-memory" {
		t.Fatalf("after dial failure, backend = %q, want %q", name, "in-memory")
	}
}

func TestInMemoryBackendRoundTrip(t *testing.T) {
	ResetBackendForTesting()
	SetEnabled(false)
	SetEnabled(true)
	defer SetEnabled(false)

	Enqueue([]byte("a"))
	Enqueue([]byte("b"))
	got := PopOldest(10)
	if len(got) != 2 || string(got[0]) != "a" || string(got[1]) != "b" {
		t.Fatalf("PopOldest returned %v", got)
	}
}

func TestRetentionPruning(t *testing.T) {
	ResetBackendForTesting()
	SetEnabled(false)
	SetEnabled(true)
	defer SetEnabled(false)
	SetRetentionSeconds(1)
	defer SetRetentionSeconds(int(defaultRetentionSeconds))

	be, ok := getBackend().(*inMemoryBackend)
	if !ok {
		t.Fatalf("expected in-memory backend")
	}
	be.mu.Lock()
	be.items = append(be.items, queueItem{enqueuedAt: time.Now().Add(-2 * time.Second), payload: []byte("old")})
	be.mu.Unlock()

	Enqueue([]byte("new"))
	got := PopOldest(10)
	if len(got) != 1 || string(got[0]) != "new" {
		t.Fatalf("expected only fresh item, got %v", got)
	}
}
