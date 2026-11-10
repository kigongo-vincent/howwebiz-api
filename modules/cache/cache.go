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
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) {
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
