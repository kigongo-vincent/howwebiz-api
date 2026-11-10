package sitecrawl_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeAPI serves two feeds, 2 tracks per page:
//
//	head: newest first (new ids are prepended)
//	tail: oldest first (new ids are appended)
type fakeAPI struct {
	mu   sync.Mutex
	feed map[string][]int
	hits atomic.Int64
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/robots.txt" {
		w.WriteHeader(404) // APIs often have none: allowed
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits.Add(1)
	ids := f.feed[r.URL.Query().Get("f")]
	o, _ := strconv.Atoi(r.URL.Query().Get("o"))
	for i := o; i < min(o+2, len(ids)); i++ {
		fmt.Fprintf(w, "%s%d\n", r.URL.Query().Get("f"), ids[i])
	}
}

type api struct{ enabled bool }

func (api) Name() string               { return "api" }
func (api) Info() catalog.ProviderInfo { return catalog.ProviderInfo{Name: "api"} }
func (a api) Enabled() bool            { return a.enabled }
func (api) Feeds() []string            { return []string{"head", "tail"} }
func (api) Tail(feed string) bool      { return feed == "tail" }
func (api) SearchAPI(context.Context, sitecrawl.Fetch, string, int) ([]catalog.Track, error) {
	return []catalog.Track{{Provider: "api", ExternalID: "s", Title: "S", Artist: "A", AudioURL: "x"}, {Provider: "api", ExternalID: "n", Title: "N", Artist: "A"}}, nil
}

func (api) FeedPage(ctx context.Context, get sitecrawl.Fetch, feed, cursor string) ([]catalog.Track, string, bool, error) {
	o, _ := strconv.Atoi(cursor)
	body, err := get(ctx, fmt.Sprintf("/feed?f=%s&o=%d", feed, o))
	if err != nil {
		return nil, "", false, err
	}
	var out []catalog.Track
	for _, line := range strings.Fields(string(body)) {
		out = append(out, catalog.Track{Provider: "api", ExternalID: line, Title: "Song " + line, Artist: "A", AudioURL: "https://x/" + line})
	}
	next := strconv.Itoa(o + len(out))
	return out, next, len(out) < 2, nil
}

func setupAPI(t *testing.T, fa *fakeAPI, enabled bool) (*sitecrawl.Crawler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&catalog.Song{}, &catalog.Source{}, &sitecrawl.Cursor{}, &sitecrawl.Probe{}, &sitecrawl.FeedCursor{},
		&crawler.CrawlRun{}, &crawler.PageFingerprint{}, &webguard.Health{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fa)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cr := sitecrawl.New(ctx, db, api{enabled: enabled}, sitecrawl.Config{
		Web: webguard.Config{BaseURL: srv.URL, UserAgent: "test", MaxConsecutive: 2, MaxPerRun: 5,
			Backoff: time.Millisecond, Cooldown: time.Hour, MaxCooldown: time.Hour},
		ProbeAhead: 3, ProbeRetryAfter: time.Hour, Refresh: time.Hour,
	}, &catalog.Ingester{DB: db, Priority: []string{"api"}})
	t.Cleanup(func() { cancel(); cr.Runner.Wait() })
	return cr, db
}

func TestFeeds(t *testing.T) {
	fa := &fakeAPI{feed: map[string][]int{"head": {5, 4, 3, 2, 1}, "tail": {1, 2, 3}}}
	cr, db := setupAPI(t, fa, true)
	count := func() (n int64) {
		db.Model(&catalog.Source{}).Where("provider = 'api'").Count(&n)
		return
	}

	// A budget of 3 pages: feeds take turns (head p1, tail p1, head p2),
	// and the walk resumes where it stopped.
	st := run(t, cr, db, sitecrawl.Options{MaxPages: 3})
	if st.PagesFetched != 3 || count() != 6 {
		t.Fatalf("run 1: stats=%+v sources=%d", st, count())
	}
	run(t, cr, db, sitecrawl.Options{})
	if count() != 8 {
		t.Fatalf("run 2: sources=%d, want all 8", count())
	}
	var cur sitecrawl.Cursor
	db.First(&cur, "site = ?", "api")
	if cur.Phase != sitecrawl.PhaseSynced {
		t.Fatalf("phase %s after every feed was walked", cur.Phase)
	}

	// New items: the head feed gets one on top (read from the start, stop
	// at the first page with nothing new); the tail feed gets one at the
	// end (resumed from its stored cursor, not re-read).
	fa.mu.Lock()
	fa.feed["head"] = append([]int{6}, fa.feed["head"]...)
	fa.feed["tail"] = append(fa.feed["tail"], 4)
	fa.mu.Unlock()
	hits := fa.hits.Load()
	st = run(t, cr, db, sitecrawl.Options{})
	if st.SongsInserted != 2 || count() != 10 {
		t.Fatalf("run 3: stats=%+v sources=%d", st, count())
	}
	// head: page 1 (6,5 → new) + page 2 (4,3 → nothing new, stop); tail: offset 3 → [4], end.
	if got := fa.hits.Load() - hits; got != 3 {
		t.Fatalf("run 3: %d requests, want 3", got)
	}

	// Live search through the API keeps playable tracks only.
	tracks, err := cr.Search(context.Background(), "q", 10)
	if err != nil || len(tracks) != 1 || tracks[0].ExternalID != "s" {
		t.Fatalf("search: %+v %v", tracks, err)
	}
}

func TestDisabledProvider(t *testing.T) {
	cr, _ := setupAPI(t, &fakeAPI{feed: map[string][]int{}}, false)
	if _, err := cr.Start("test", sitecrawl.Options{}); !errors.Is(err, sitecrawl.ErrNotConfigured) {
		t.Fatalf("start: %v", err)
	}
	if _, err := cr.Search(context.Background(), "q", 5); !errors.Is(err, catalog.ErrUnavailable) {
		t.Fatalf("search: %v", err)
	}
}
