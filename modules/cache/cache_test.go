package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestGetSetBump(t *testing.T) {
	mr := miniredis.RunT(t)
	c := New(mr.Addr(), "t:")
	ctx := context.Background()

	gen := c.Generation(ctx, "ns")
	c.Set(ctx, "k:"+gen, map[string]int{"a": 1}, time.Minute)
	var got map[string]int
	if !c.Get(ctx, "k:"+gen, &got) || got["a"] != 1 {
		t.Fatalf("get = %v", got)
	}
	if c.Get(ctx, "missing", &got) {
		t.Fatal("hit on a missing key")
	}
	c.Bump(ctx, "ns")
	if next := c.Generation(ctx, "ns"); next == gen || c.Get(ctx, "k:"+next, &got) {
		t.Fatalf("bump did not invalidate: gen %s -> %s", gen, next)
	}
	mr.FastForward(2 * time.Minute)
	if c.Get(ctx, "k:"+gen, &got) {
		t.Fatal("entry outlived its ttl")
	}
}

// A dead Redis is a miss, and after the first failure calls skip it
// entirely: the API never waits on a cache that isn't there.
func TestDownRedisIsFastMiss(t *testing.T) {
	mr := miniredis.RunT(t)
	c := New(mr.Addr(), "t:")
	mr.Close()
	ctx := context.Background()
	var v int
	if c.Get(ctx, "k", &v) {
		t.Fatal("hit from a closed server")
	}
	start := time.Now()
	for range 100 {
		c.Get(ctx, "k", &v)
		c.Set(ctx, "k", 1, time.Minute)
		c.Generation(ctx, "ns")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("300 calls on a tripped breaker took %v", d)
	}
}

func TestNilCacheIsNoop(t *testing.T) {
	var c *Cache
	ctx := context.Background()
	var v int
	c.Set(ctx, "k", 1, time.Minute)
	if c.Get(ctx, "k", &v) || c.Generation(ctx, "ns") != "0" {
		t.Fatal("nil cache returned data")
	}
	c.Bump(ctx, "ns")
}
