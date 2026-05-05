package redisqueue

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

// RedisBackendConfig captures the connection parameters for an external
// Redis/Valkey-backed queue.
type RedisBackendConfig struct {
	Address   string
	Password  string
	DB        int
	KeyPrefix string
}

const (
	defaultRedisKeyPrefix = "cliproxy:usage"
	redisListSuffix       = ":queue"
	redisDialTimeout      = 5 * time.Second
	redisPingTimeout      = 5 * time.Second
	maxPruneIterations    = 64
	tsPrefixLen           = 8
)

// redisBackend buffers payloads in a Redis LIST. Newest entries are pushed to
// the head with LPUSH, oldest entries are popped from the tail with RPOP so
// PopOldest semantics match the in-memory backend. Each payload is stored with
// an 8-byte big-endian Unix-nano timestamp prefix, allowing retention pruning
// without a sidecar key.
type redisBackend struct {
	client *redis.Client
	key    string
}

// ConfigureRedisBackend dials the configured Redis/Valkey instance, pings it,
// and installs the redis backend. On failure the previous backend is kept and
// an error is returned so callers can fall back to in-memory mode.
func ConfigureRedisBackend(cfg RedisBackendConfig) error {
	address := strings.TrimSpace(cfg.Address)
	if address == "" {
		return errors.New("redisqueue: external backend address is empty")
	}
	prefix := strings.TrimSpace(cfg.KeyPrefix)
	if prefix == "" {
		prefix = defaultRedisKeyPrefix
	}
	client := redis.NewClient(&redis.Options{
		Addr:        address,
		Password:    cfg.Password,
		DB:          cfg.DB,
		DialTimeout: redisDialTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return fmt.Errorf("redisqueue: ping %s: %w", address, err)
	}
	prev := setBackend(&redisBackend{
		client: client,
		key:    prefix + redisListSuffix,
	})
	if prev != nil {
		_ = prev.Close()
	}
	log.Infof("redisqueue: external Redis/Valkey backend enabled (addr=%s, key=%s)", address, prefix+redisListSuffix)
	return nil
}

func encodePayload(now time.Time, payload []byte) []byte {
	out := make([]byte, tsPrefixLen+len(payload))
	binary.BigEndian.PutUint64(out[:tsPrefixLen], uint64(now.UnixNano()))
	copy(out[tsPrefixLen:], payload)
	return out
}

func decodePayload(raw []byte) (time.Time, []byte, bool) {
	if len(raw) < tsPrefixLen {
		return time.Time{}, nil, false
	}
	ts := int64(binary.BigEndian.Uint64(raw[:tsPrefixLen]))
	return time.Unix(0, ts), raw[tsPrefixLen:], true
}

func (b *redisBackend) Enqueue(payload []byte) {
	now := time.Now()
	encoded := encodePayload(now, payload)
	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()
	if err := b.client.LPush(ctx, b.key, encoded).Err(); err != nil {
		log.Warnf("redisqueue: LPUSH failed: %v", err)
		return
	}
	b.pruneTail(ctx, now)
}

// pruneTail removes expired records from the tail (oldest end) of the list.
// Bounded iteration count prevents a hot loop on misconfigured retention.
func (b *redisBackend) pruneTail(ctx context.Context, now time.Time) {
	cutoff := now.Add(-time.Duration(currentRetentionSeconds()) * time.Second)
	for i := 0; i < maxPruneIterations; i++ {
		raw, err := b.client.LIndex(ctx, b.key, -1).Bytes()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				log.Warnf("redisqueue: LINDEX failed: %v", err)
			}
			return
		}
		ts, _, ok := decodePayload(raw)
		if !ok || !ts.Before(cutoff) {
			return
		}
		if err := b.client.RPop(ctx, b.key).Err(); err != nil && !errors.Is(err, redis.Nil) {
			log.Warnf("redisqueue: RPOP prune failed: %v", err)
			return
		}
	}
}

func (b *redisBackend) PopOldest(count int) [][]byte {
	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()
	now := time.Now()
	b.pruneTail(ctx, now)
	cutoff := now.Add(-time.Duration(currentRetentionSeconds()) * time.Second)

	values, err := b.client.RPopCount(ctx, b.key, count).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			log.Warnf("redisqueue: RPOP COUNT failed: %v", err)
		}
		return nil
	}
	out := make([][]byte, 0, len(values))
	for _, v := range values {
		ts, payload, ok := decodePayload([]byte(v))
		if !ok {
			continue
		}
		if ts.Before(cutoff) {
			continue
		}
		out = append(out, append([]byte(nil), payload...))
	}
	return out
}

func (b *redisBackend) Clear() {
	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()
	if err := b.client.Del(ctx, b.key).Err(); err != nil && !errors.Is(err, redis.Nil) {
		log.Warnf("redisqueue: DEL failed: %v", err)
	}
}

func (b *redisBackend) Close() error {
	if b.client == nil {
		return nil
	}
	return b.client.Close()
}
