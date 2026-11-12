package webguard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testFetcher(t *testing.T, block *atomic.Bool) (*Fetcher, *atomic.Int64) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Health{}); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	_ = db.Callback().Create().After("gorm:create").Register("count", func(*gorm.DB) { writes.Add(1) })
	_ = db.Callback().Update().After("gorm:update").Register("count", func(*gorm.DB) { writes.Add(1) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if block.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	f := New(db, "site", Config{BaseURL: srv.URL, UserAgent: "test", MaxConsecutive: 1, MaxPerRun: 5,
		Backoff: time.Millisecond, Cooldown: time.Hour, MaxCooldown: time.Hour})
	f.SaveEvery = time.Hour
	return f, &writes
}

// Routine requests are counted in memory and written in one go; a block
// is written at once and never undone by counters pending from before it.
func TestGuardBatchesCountersAndKeepsBlocks(t *testing.T) {
	var block atomic.Bool
	f, writes := testFetcher(t, &block)
	ctx := context.Background()
	for range 20 {
		if _, err := f.Get(ctx, "/page"); err != nil {
			t.Fatal(err)
		}
	}
	// the first request creates the row (an update finding none, then an
	// insert); the other 19 wait
	if n := writes.Load(); n > 2 {
		t.Fatalf("%d writes for 20 requests, want at most 2", n)
	}
	block.Store(true)
	if _, err := f.Get(ctx, "/page"); err == nil {
		t.Fatal("want the circuit to open")
	}
	h, _ := f.Health()
	if h.BlockedUntil == nil || h.TotalRequests != 21 || h.TotalOK != 20 || h.Trips != 1 {
		t.Fatalf("after the block: %+v", h)
	}
	// counters pending afterwards never clear the block
	f.mu.Lock()
	_ = f.note(true, time.Now())
	_ = f.flush()
	f.mu.Unlock()
	h, _ = f.Health()
	if h.BlockedUntil == nil || h.TotalRequests != 22 {
		t.Fatalf("a flush undid the block: %+v", h)
	}
	if ok, _ := f.CircuitOpen(); !ok {
		t.Fatal("circuit should be open")
	}
}
