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

// fakeSite: a newest-first listing ("id|title" lines, 3 per page, no
// audio) and song pages by id that embed the next id's song too.
type fakeSite struct {
	mu       sync.Mutex
	listed   []int        // newest first
	exists   map[int]bool // every real song id
	blocked  bool
	songHits atomic.Int64
	listHits atomic.Int64
}

func (f *fakeSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/robots.txt" {
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
		return
	}
	if f.blocked {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(403)
		return
	}
	switch {
	case r.URL.Path == "/list":
		f.listHits.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		for i := (page - 1) * 3; i < min(page*3, len(f.listed)); i++ {
			fmt.Fprintf(w, "%d|Song %d\n", f.listed[i], f.listed[i])
		}
	case strings.HasPrefix(r.URL.Path, "/song/"):
		f.songHits.Add(1)
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/song/"))
		for _, x := range []int{id, id + 1} {
			if f.exists[x] {
				fmt.Fprintf(w, "%d|Song %d|audio\n", x, x)
			}
		}
	case r.URL.Path == "/search":
		fmt.Fprintf(w, "%d|Song %d\n", 1, 1)
	}
}

// site implements sitecrawl.Site, Prober and Searchable for the fake.
type site struct{}

func (site) Name() string                                    { return "fake" }
func (site) Info() catalog.ProviderInfo                      { return catalog.ProviderInfo{Name: "fake"} }
func (site) ListPath(p int) string                           { return fmt.Sprintf("/list?page=%d", p) }
func (site) SongPath(id int) string                          { return fmt.Sprintf("/song/%d", id) }
func (site) SearchPath(q string) string                      { return "/search?q=" + q }
func (s site) ParseList(b []byte) ([]catalog.Track, error)   { return parse(b), nil }
func (s site) ParseSearch(b []byte) ([]catalog.Track, error) { return parse(b), nil }
func (s site) ParseSong(b []byte, id int) ([]catalog.Track, error) {
	t := parse(b)
	if len(t) == 0 || t[0].ExternalID != strconv.Itoa(id) {
		return nil, sitecrawl.ErrNotFound
	}
	return t, nil
}

func parse(b []byte) []catalog.Track {
	var out []catalog.Track
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Split(line, "|")
		if len(f) < 2 {
			continue
		}
		t := catalog.Track{Provider: "fake", ExternalID: f[0], Title: f[1], Artist: "Artist"}
		if len(f) > 2 {
			t.AudioURL = "https://fake/" + f[0] + ".mp3"
		}
		out = append(out, t)
	}
	return out
}

func setup(t *testing.T, fs *fakeSite) (*sitecrawl.Crawler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&catalog.Song{}, &catalog.Source{}, &sitecrawl.Cursor{}, &sitecrawl.Probe{},
		&crawler.CrawlRun{}, &crawler.PageFingerprint{}, &webguard.Health{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cr := sitecrawl.New(ctx, db, site{}, sitecrawl.Config{
		Web: webguard.Config{BaseURL: srv.URL, UserAgent: "test", MaxConsecutive: 2, MaxPerRun: 5,
			Backoff: time.Millisecond, Cooldown: time.Hour, MaxCooldown: time.Hour},
		ProbeAhead: 3, ProbeRetryAfter: time.Hour, Refresh: time.Hour, SearchEnrich: 5,
	}, &catalog.Ingester{DB: db, Priority: []string{"fake"}})
	t.Cleanup(func() { cancel(); cr.Runner.Wait() })
	return cr, db
}

func run(t *testing.T, cr *sitecrawl.Crawler, db *gorm.DB, o sitecrawl.Options) crawler.Stats {
	t.Helper()
	r, err := cr.Start("test", o)
	if err != nil {
		t.Fatal(err)
	}
	cr.Runner.Wait()
	var done crawler.CrawlRun
	db.First(&done, r.ID)
	if len(done.Errors) > 0 {
		t.Fatalf("run errors: %v", done.Errors)
	}
	return done.Stats.Data()
}

func TestCrawlerListingSweepAndSync(t *testing.T) {
	fs := &fakeSite{exists: map[int]bool{}}
	// Real songs 1..12 except 5; the listing shows 10..3 (not 1, 2, 11, 12).
	for id := 1; id <= 12; id++ {
		if id != 5 {
			fs.exists[id] = true
		}
	}
	for id := 10; id >= 3; id-- {
		if id != 5 {
			fs.listed = append(fs.listed, id)
		}
	}
	cr, db := setup(t, fs)
	count := func(where string) (n int64) {
		db.Model(&catalog.Source{}).Where("provider = 'fake'" + where).Count(&n)
		return
	}

	// Listing cards have no audio, so the listing is discovery only (page 1
	// → newest id 10); the sweep fetches song pages, which embed the next
	// id, so ids completed by an earlier page are not fetched again.
	st := run(t, cr, db, sitecrawl.Options{})
	if count("") != 11 || count(" AND audio_url <> ''") != 11 || st.PagesFetched != 1 {
		t.Fatalf("run 1: sources=%d withAudio=%d stats=%+v", count(""), count(" AND audio_url <> ''"), st)
	}
	// Probed: 1 (gives 2), 3 (gives 4), 5 (missing), 6 (gives 7), 8 (gives 9),
	// 10 (gives 11), then ahead from 11: 11, 12 found, 13-15 missing.
	if fs.songHits.Load() != 11 {
		t.Fatalf("run 1: %d song pages fetched, want 11", fs.songHits.Load())
	}
	var cur sitecrawl.Cursor
	db.First(&cur, "site = ?", "fake")
	if cur.Phase != sitecrawl.PhaseSynced {
		t.Fatalf("run 1: phase %s", cur.Phase)
	}

	// A new song at the top: synced runs stop at the first listing page
	// with nothing new, fetch the new song's page, and probe ahead.
	fs.mu.Lock()
	fs.exists[13] = true
	fs.listed = append([]int{13}, fs.listed...)
	fs.mu.Unlock()
	lists, songs := fs.listHits.Load(), fs.songHits.Load()
	st = run(t, cr, db, sitecrawl.Options{})
	if st.SongsInserted != 1 || fs.listHits.Load()-lists != 1 || count(" AND audio_url <> ''") != 12 {
		t.Fatalf("run 2: listing pages=%d stats=%+v", fs.listHits.Load()-lists, st)
	}
	if got := fs.songHits.Load() - songs; got != 1+3 { // song 13, then 14-16 ahead
		t.Fatalf("run 2: %d song pages, want 4", got)
	}

	// Live search: the search page lists song 1 without audio, so its
	// song page is fetched (it also carries song 2); only tracks with
	// audio come back.
	tracks, err := cr.Search(context.Background(), "song", 10)
	if err != nil || len(tracks) != 2 || tracks[0].ExternalID != "1" || tracks[0].AudioURL == "" || tracks[1].AudioURL == "" {
		t.Fatalf("search: %+v %v", tracks, err)
	}
	fs.mu.Lock()
	fs.blocked = true
	fs.mu.Unlock()
	_, err = cr.Search(context.Background(), "song", 10)
	if !errors.Is(err, catalog.ErrUnavailable) {
		t.Fatalf("blocked search: want ErrUnavailable, got %v", err)
	}
	if _, err := cr.Start("test", sitecrawl.Options{}); !errors.Is(err, webguard.ErrCircuitOpen) {
		t.Fatalf("crawl while paused: %v", err)
	}
}

// smSite is a sitemap-only site (no listing, no search): /sitemap.xml is
// an index pointing at /sitemap/songs, whose song pages embed one more song.
type smSite struct{}

func (smSite) Name() string               { return "smfake" }
func (smSite) Info() catalog.ProviderInfo { return catalog.ProviderInfo{Name: "smfake"} }
func (smSite) SitemapPaths() []string     { return []string{"/sitemap.xml"} }
func (smSite) IsSongPage(p string) bool   { return strings.HasPrefix(p, "/songs/") }
func (smSite) ParseSongPage(b []byte) ([]catalog.Track, error) {
	t := parse(b)
	if len(t) == 0 {
		return nil, sitecrawl.ErrNotFound
	}
	return t, nil
}

type smServer struct {
	mu      sync.Mutex
	lastmod map[int]string // song page id → lastmod
	hits    map[string]int
}

func (s *smServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[r.URL.Path]++
	switch {
	case r.URL.Path == "/robots.txt":
		fmt.Fprint(w, "User-agent: *\nDisallow: /*?*page=\nCrawl-delay: 0\n")
	case r.URL.Path == "/sitemap.xml":
		fmt.Fprint(w, `<?xml version="1.0"?><sitemapindex><sitemap><loc>http://x/sitemap/videos</loc></sitemap><sitemap><loc>http://x/sitemap/songs</loc></sitemap></sitemapindex>`)
	case r.URL.Path == "/sitemap/videos":
		w.WriteHeader(500) // a broken child sitemap must not stop the others
	case r.URL.Path == "/sitemap/songs":
		fmt.Fprint(w, `<?xml version="1.0"?><urlset>`)
		for id, lm := range s.lastmod {
			fmt.Fprintf(w, `<url><loc>http://x/songs/s%d</loc><lastmod>%s</lastmod></url>`, id, lm)
		}
		fmt.Fprint(w, `<url><loc>http://x/artists/a</loc></url></urlset>`)
	case strings.HasPrefix(r.URL.Path, "/songs/s"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/songs/s"))
		if id == 3 {
			return // a page without a song
		}
		fmt.Fprintf(w, "%d|Song %d|audio\n%d|Related %d|audio\n", id, id, 100+id, id)
	}
}

func TestCrawlerSitemap(t *testing.T) {
	sv := &smServer{lastmod: map[int]string{1: "2026-11-01T00:00:00Z", 2: "2026-11-02T00:00:00Z", 3: "2026-11-03T00:00:00Z"}, hits: map[string]int{}}
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	db.AutoMigrate(&catalog.Song{}, &catalog.Source{}, &sitecrawl.Cursor{}, &sitecrawl.Probe{}, &sitecrawl.Page{},
		&crawler.CrawlRun{}, &crawler.PageFingerprint{}, &webguard.Health{})
	srv := httptest.NewServer(sv)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := sitecrawl.New(ctx, db, smSite{}, sitecrawl.Config{
		Web: webguard.Config{BaseURL: srv.URL, UserAgent: "test", MaxConsecutive: 2, MaxPerRun: 5,
			Backoff: time.Millisecond, Cooldown: time.Hour, MaxCooldown: time.Hour},
		ProbeAhead: 3, Refresh: time.Hour}, &catalog.Ingester{DB: db, Priority: []string{"smfake"}})
	defer cr.Runner.Wait()
	sources := func() (n int64) { db.Model(&catalog.Source{}).Count(&n); return }
	// Every run reports the broken videos sitemap and nothing else.
	runSM := func() {
		t.Helper()
		r, err := cr.Start("test", sitecrawl.Options{})
		if err != nil {
			t.Fatal(err)
		}
		cr.Runner.Wait()
		var done crawler.CrawlRun
		db.First(&done, r.ID)
		if len(done.Errors) != 1 || !strings.Contains(done.Errors[0], "/sitemap/videos") {
			t.Fatalf("errors: %v", done.Errors)
		}
	}

	r, _ := cr.Start("test", sitecrawl.Options{})
	cr.Runner.Wait()
	var first crawler.CrawlRun
	db.First(&first, r.ID)
	st := first.Stats.Data()
	// 3 song pages (one empty); each real page also yields its related song.
	// The broken videos sitemap is reported, not fatal.
	if sources() != 4 || st.DetailsFetched != 3 || sv.hits["/artists/a"] != 0 || len(first.Errors) != 1 {
		t.Fatalf("run 1: sources=%d stats=%+v hits=%v", sources(), st, sv.hits)
	}

	// Nothing modified: only the sitemaps are read.
	before := sv.hits["/songs/s1"] + sv.hits["/songs/s2"] + sv.hits["/songs/s3"]
	runSM()
	if after := sv.hits["/songs/s1"] + sv.hits["/songs/s2"] + sv.hits["/songs/s3"]; after != before {
		t.Fatalf("run 2 refetched unchanged pages: %d → %d", before, after)
	}

	// One page modified, one added: exactly those are fetched.
	sv.mu.Lock()
	sv.lastmod[2], sv.lastmod[4] = "2026-11-09T00:00:00Z", "2026-11-09T00:00:00Z"
	sv.mu.Unlock()
	runSM()
	if sv.hits["/songs/s1"] != 1 || sv.hits["/songs/s2"] != 2 || sv.hits["/songs/s4"] != 1 || sources() != 6 {
		t.Fatalf("run 3: hits=%v sources=%d", sv.hits, sources())
	}
}

// Enrichment stops before the deadline and returns what it has, instead
// of letting the caller's timeout discard everything.
func TestSearchEnrichRespectsDeadline(t *testing.T) {
	fs := &fakeSite{exists: map[int]bool{}}
	for id := 1; id <= 20; id++ {
		fs.exists[id] = true
	}
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" { // ten results without audio
			for id := 1; id <= 19; id += 2 {
				fmt.Fprintf(w, "%d|Song %d\n", id, id)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/song/") {
			time.Sleep(400 * time.Millisecond)
		}
		fs.ServeHTTP(w, r)
	})
	cr, _ := setup(t, fs)
	srv := httptest.NewServer(slow)
	defer srv.Close()
	cr.Web.Cfg.BaseURL = srv.URL
	cr.Cfg.SearchEnrich = 10

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	tracks, err := cr.Search(ctx, "song", 50)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) >= 2*time.Second {
		t.Fatalf("search overran its deadline: %v", time.Since(start))
	}
	if len(tracks) == 0 || len(tracks) >= 20 {
		t.Fatalf("want a partial, non-empty result, got %d tracks", len(tracks))
	}
	for _, tr := range tracks {
		if tr.AudioURL == "" {
			t.Fatalf("track without audio returned: %+v", tr)
		}
	}
}
