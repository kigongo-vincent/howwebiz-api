package httpcache

import (
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/cache"
)

func setup(t *testing.T) (*fiber.App, *atomic.Bool, *atomic.Int32, *cache.Cache) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := cache.New(mr.Addr(), "t:")
	fail := &atomic.Bool{}
	calls := &atomic.Int32{}
	app := fiber.New()
	v1 := app.Group("/api/v1")
	v1.Use(New(Options{Cache: c, Prefixes: []string{"/api/v1/songs"}, Skip: []string{"/status"}, FreshTTL: time.Minute,
		Shuffled: func(path string, q url.Values) bool { return q.Get("sort") == "discover" && q.Get("seed") == "" }}))
	v1.Get("/songs", func(ctx *fiber.Ctx) error {
		calls.Add(1)
		if fail.Load() {
			return fiber.NewError(fiber.StatusServiceUnavailable, "db timeout")
		}
		return ctx.JSON(fiber.Map{"q": ctx.Query("q"), "n": calls.Load()})
	})
	v1.Get("/songs/status", func(ctx *fiber.Ctx) error { calls.Add(1); return ctx.JSON(fiber.Map{}) })
	v1.Get("/songs/missing", func(ctx *fiber.Ctx) error { return fiber.ErrNotFound })
	return app, fail, calls, c
}

func get(t *testing.T, app *fiber.App, url string) (int, string, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", url, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Cache"), string(b)
}

func TestFreshHitAndQueryOrder(t *testing.T) {
	app, _, calls, _ := setup(t)
	if st, how, _ := get(t, app, "/api/v1/songs?q=kenzo&page=1"); st != 200 || how != "MISS" {
		t.Fatalf("first: %d %q", st, how)
	}
	// same request, parameters in another order: one entry
	if st, how, body := get(t, app, "/api/v1/songs?page=1&q=kenzo"); st != 200 || how != "HIT" || body != `{"n":1,"q":"kenzo"}` {
		t.Fatalf("second: %d %q %s", st, how, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times", calls.Load())
	}
}

func TestStaleWhenHandlerFails(t *testing.T) {
	app, fail, _, c := setup(t)
	get(t, app, "/api/v1/songs?q=a")
	c.Del(context.Background(), "http:fresh:/api/v1/songs?q=a") // fresh copy expired
	fail.Store(true)
	st, how, body := get(t, app, "/api/v1/songs?q=a")
	if st != 200 || how != "STALE" || body != `{"n":1,"q":"a"}` {
		t.Fatalf("stale: %d %q %s", st, how, body)
	}
	// nothing stored for this one: the failure goes through
	if st, _, _ := get(t, app, "/api/v1/songs?q=never"); st != 503 {
		t.Fatalf("no stale copy: %d", st)
	}
}

func TestSkipsAndClientErrors(t *testing.T) {
	app, _, calls, _ := setup(t)
	get(t, app, "/api/v1/songs/status")
	get(t, app, "/api/v1/songs/status")
	if calls.Load() != 2 {
		t.Fatalf("status page cached: %d calls", calls.Load())
	}
	if st, how, _ := get(t, app, "/api/v1/songs/missing"); st != 404 || how != "" {
		t.Fatalf("404: %d %q", st, how)
	}
}

func TestWarmerRebuildsHotPages(t *testing.T) {
	app, _, calls, c := setup(t)
	get(t, app, "/api/v1/songs?q=hot")
	get(t, app, "/api/v1/songs?q=hot") // hit: counted twice, handler ran once
	get(t, app, "/api/v1/songs?q=cold")
	before := calls.Load()
	Warmer{App: app, Cache: c, Timeout: time.Second}.round(context.Background())
	if calls.Load() != before+1 {
		t.Fatalf("warmer ran %d handlers, want 1 (only the hot page)", calls.Load()-before)
	}
	if _, how, body := get(t, app, "/api/v1/songs?q=hot"); how != "HIT" || body != `{"n":3,"q":"hot"}` {
		t.Fatalf("after warm: %q %s", how, body)
	}
}

func TestShuffledNeverServedFresh(t *testing.T) {
	app, fail, calls, _ := setup(t)
	get(t, app, "/api/v1/songs?sort=discover")
	if _, how, _ := get(t, app, "/api/v1/songs?sort=discover"); how != "MISS" || calls.Load() != 2 {
		t.Fatalf("an unseeded shuffle was served from cache: %q, %d calls", how, calls.Load())
	}
	// with a seed, the same shuffle is cacheable
	get(t, app, "/api/v1/songs?sort=discover&seed=abc")
	if _, how, _ := get(t, app, "/api/v1/songs?sort=discover&seed=abc"); how != "HIT" {
		t.Fatalf("seeded shuffle: %q", how)
	}
	// still a stale fallback when the database fails
	fail.Store(true)
	if st, how, _ := get(t, app, "/api/v1/songs?sort=discover"); st != 200 || how != "STALE" {
		t.Fatalf("stale fallback: %d %q", st, how)
	}
}
