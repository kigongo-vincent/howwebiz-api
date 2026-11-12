// Package cache is a small JSON cache on Redis that the API can always live
// without: a nil *Cache, an unreachable server or any Redis error is a cache
// miss, never a failed request. After an error Redis is left alone for a
// cool-down, so a dead server can't add its timeout to every request.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultAddr is where Redis runs in production: inside the API's own
// container, on loopback only (see docker-entrypoint.sh). Dev's
// docker-compose publishes its Redis on the same address.
const DefaultAddr = "127.0.0.1:6379"

const coolDown = 10 * time.Second

type Cache struct {
	rdb       *redis.Client
	prefix    string
	downUntil atomic.Int64 // unix nanos; skip Redis until then
}

// New connects lazily (the first command dials), so the API starts even
// when Redis is not up yet.
func New(addr, prefix string) *Cache {
	return &Cache{prefix: prefix, rdb: redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  250 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		PoolSize:     32,
		MaxRetries:   -1, // a miss is cheaper than a retry
	})}
}

// Ping reports whether Redis answers (for startup logs and health).
func (c *Cache) Ping(ctx context.Context) error {
	if c == nil {
		return errors.New("cache disabled")
	}
	return c.rdb.Ping(ctx).Err()
}

func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	return c.rdb.Close()
}

func (c *Cache) usable() bool {
	return c != nil && time.Now().UnixNano() >= c.downUntil.Load()
}

// failed trips the breaker (logging once per trip) unless err is a plain miss.
func (c *Cache) failed(err error) {
	// a miss, or the request went away: Redis itself is fine
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	prev := c.downUntil.Swap(time.Now().Add(coolDown).UnixNano())
	if time.Now().UnixNano() >= prev {
		slog.Warn("cache: redis unavailable, serving uncached", "retryIn", coolDown, "err", err)
	}
}

// Get decodes the value at key into v; false on a miss or any error.
func (c *Cache) Get(ctx context.Context, key string, v any) bool {
	if !c.usable() {
		return false
	}
	b, err := c.rdb.Get(ctx, c.prefix+key).Bytes()
	if err != nil {
		c.failed(err)
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// Set stores v as JSON for ttl (best effort).
func (c *Cache) Set(ctx context.Context, key string, v any, ttl time.Duration) {
	if !c.usable() || ttl <= 0 {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.failed(c.rdb.Set(ctx, c.prefix+key, b, ttl).Err())
}

// Generation returns the namespace's current generation. Keys built with it
// are all invalidated at once by Bump (old entries just expire).
func (c *Cache) Generation(ctx context.Context, ns string) string {
	if !c.usable() {
		return "0"
	}
	n, err := c.rdb.Get(ctx, c.prefix+"gen:"+ns).Int64()
	if err != nil {
		c.failed(err)
	}
	return strconv.FormatInt(n, 10)
}

// Bump invalidates every key built with the namespace's generation.
func (c *Cache) Bump(ctx context.Context, ns string) {
	if !c.usable() {
		return
	}
	c.failed(c.rdb.Incr(ctx, c.prefix+"gen:"+ns).Err())
}

// GetBytes returns the raw value at key; false on a miss or any error.
func (c *Cache) GetBytes(ctx context.Context, key string) ([]byte, bool) {
	if !c.usable() {
		return nil, false
	}
	b, err := c.rdb.Get(ctx, c.prefix+key).Bytes()
	if err != nil {
		c.failed(err)
		return nil, false
	}
	return b, true
}

// SetBytes stores a raw value for ttl (best effort).
func (c *Cache) SetBytes(ctx context.Context, key string, b []byte, ttl time.Duration) {
	if !c.usable() || ttl <= 0 {
		return
	}
	c.failed(c.rdb.Set(ctx, c.prefix+key, b, ttl).Err())
}

// SetNX stores v only when key is absent; true when it was stored (a
// once-per-ttl gate across restarts). False when Redis is unavailable.
func (c *Cache) SetNX(ctx context.Context, key string, ttl time.Duration) bool {
	if !c.usable() {
		return false
	}
	ok, err := c.rdb.SetNX(ctx, c.prefix+key, "1", ttl).Result()
	c.failed(err)
	return ok
}

// Exists reports whether key is set (false when Redis is unavailable).
func (c *Cache) Exists(ctx context.Context, key string) bool {
	if !c.usable() {
		return false
	}
	n, err := c.rdb.Exists(ctx, c.prefix+key).Result()
	c.failed(err)
	return n > 0
}

// Del removes keys (best effort).
func (c *Cache) Del(ctx context.Context, keys ...string) {
	if !c.usable() || len(keys) == 0 {
		return
	}
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = c.prefix + k
	}
	c.failed(c.rdb.Del(ctx, full...).Err())
}

// ZIncr adds 1 to member's score in the sorted set at key, which expires
// after ttl (popularity counters that fade on their own).
func (c *Cache) ZIncr(ctx context.Context, key, member string, ttl time.Duration) {
	if !c.usable() {
		return
	}
	k := c.prefix + key
	p := c.rdb.Pipeline()
	p.ZIncrBy(ctx, k, 1, member)
	p.Expire(ctx, k, ttl)
	_, err := p.Exec(ctx)
	c.failed(err)
}

// ZTop returns the n highest-scored members of each sorted set, summed.
func (c *Cache) ZTop(ctx context.Context, n int, keys ...string) map[string]float64 {
	out := map[string]float64{}
	if !c.usable() {
		return out
	}
	for _, key := range keys {
		zs, err := c.rdb.ZRevRangeWithScores(ctx, c.prefix+key, 0, int64(n-1)).Result()
		if err != nil {
			c.failed(err)
			return out
		}
		for _, z := range zs {
			if m, ok := z.Member.(string); ok {
				out[m] += z.Score
			}
		}
	}
	return out
}
