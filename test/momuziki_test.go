package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/momuziki"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

// fakeMomuziki mirrors momuziki.com's markup (see modules/momuziki/testdata).
type fakeMomuziki struct {
	mu        sync.Mutex
	cats      map[uint][]uint // category → song ids, newest first
	catOrder  []uint
	exists    map[uint]bool // every real song id
	plays     map[uint]int
	hits      map[string]int
	blockAll  bool
	songProbe int // requests to song.php
}

const momuzikiPerPage = 3

func (f *fakeMomuziki) row(id uint) string {
	return fmt.Sprintf(`<div class="flex items-center justify-between p-4 rounded-lg">
		<div class="flex items-center space-x-4"><img src="music/images/thumbs/%d_thumb.jpg" alt="Song %d">
		<div><a href="song.php?id=%d" class="hover:text-red-500"><h3 class="text-lg">Song %d</h3></a><p class="text-gray-400">Artist %d</p></div></div>
		<div class="flex space-x-4"><a href="https://momuziki.com/music/uploads/Artist %d - Song %d (Audio).mp3" download>Download</a>
		<a href="song.php?id=%d">Play</a></div></div>`, id, id, id, id, id%3, id%3, id, id)
}

func (f *fakeMomuziki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/robots.txt" {
		w.Write([]byte("User-agent: *\nContent-Signal: search=yes,ai-train=no\nAllow: /\n\nUser-agent: GPTBot\nDisallow: /\n"))
		return
	}
	key := r.URL.Path + "?" + r.URL.RawQuery
	f.hits[key]++
	if f.blockAll {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(challengeHTML))
		return
	}
	q := r.URL.Query()
	var b strings.Builder
	b.WriteString(`<html><head><title>Mo Muziki UG</title></head><body><nav><a href="category.php?id=8">nav</a></nav><main>`)
	switch r.URL.Path {
	case "/categories.php":
		for _, c := range f.catOrder {
			fmt.Fprintf(&b, `<div><a href="category.php?id=%d"><img src="music/images/thumbs/c%d.jpg"><h3>Category %d</h3></a></div>`, c, c, c)
		}
	case "/category.php":
		id, _ := strconv.Atoi(q.Get("id"))
		page, _ := strconv.Atoi(q.Get("page"))
		page = max(page, 1)
		ids := f.cats[uint(id)]
		for i := (page - 1) * momuzikiPerPage; i < min(page*momuzikiPerPage, len(ids)); i++ {
			b.WriteString(f.row(ids[i]))
		}
	case "/song.php":
		f.songProbe++
		id, _ := strconv.Atoi(q.Get("id"))
		if f.exists[uint(id)] {
			fmt.Fprintf(&b, `<section><div style="background-image: url('music/images/%d_big.jpeg');"><div class="text-center">
				<h1 class="text-2xl">Song %d</h1><p class="text-red-400">Artist %d</p><p><i></i> %d Plays</p></div></div></section>
				<audio id="audio" src="https://momuziki.com/music/uploads/Artist %d - Song %d (Audio).mp3"></audio>`, id, id, id%3, f.plays[uint(id)], id%3, id)
		} else {
			b.WriteString(`<section><h1 class="text-2xl"></h1></section>`)
		}
	case "/search.php":
		needle := strings.ToLower(q.Get("query"))
		for id := uint(1); id <= 100; id++ {
			if f.exists[id] && strings.Contains(strings.ToLower(fmt.Sprintf("Song %d Artist %d", id, id%3)), needle) {
				b.WriteString(f.row(id))
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	b.WriteString(`</main></body></html>`)
	w.Write([]byte(b.String()))
}

func TestMomuziki(t *testing.T) {
	site := &fakeMomuziki{
		cats: map[uint][]uint{
			8:  {20, 18, 15, 12, 9, 6, 3},
			22: {19, 17, 14, 11, 8, 5, 2},
		},
		catOrder: []uint{22, 8}, // the crawler sorts by id
		exists:   map[uint]bool{}, plays: map[uint]int{}, hits: map[string]int{},
	}
	// 1..23 exist except 4, 13, 22; 1, 7, 10, 16 are in no category;
	// 21 and 23 sit above the highest listed id (20), like new uploads.
	for id := uint(1); id <= 23; id++ {
		if id != 4 && id != 13 && id != 22 {
			site.exists[id], site.plays[id] = true, int(id)*10
		}
	}
	srv := httptest.NewServer(site)
	defer srv.Close()

	prev := os.Getenv("DB_PATH")
	os.Setenv("DB_PATH", filepath.Join(t.TempDir(), "momuziki.db"))
	db := config.ConnectDB()
	os.Setenv("DB_PATH", prev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := momuziki.NewService(ctx, db, momuziki.Config{
		Web: webguard.Config{BaseURL: srv.URL, UserAgent: "howwebiz-test/1.0", MaxConsecutive: 3, MaxPerRun: 5,
			Backoff: time.Millisecond, Cooldown: time.Hour, MaxCooldown: 4 * time.Hour},
		DetailTTL: time.Hour, ProbeAhead: 3, ProbeRetryAfter: 24 * time.Hour, CategoryRefresh: time.Hour, MaxBrokenSongs: 5,
	})
	defer svc.Runner.Wait()
	app := config.SetupApplication()
	config.RegisterAppRoutes(config.Dependencies{DB: db, App: app, Momuziki: svc,
		Crawler: crawler.NewEngine(ctx, db, crawler.ConfigFromEnv())})

	call := func(method, path, body string) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rd)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := app.Test(req, 10000)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, data
	}
	crawl := func(body string) (crawler.CrawlRun, crawler.Stats) {
		t.Helper()
		code, data := call(http.MethodPost, "/api/v1/momuziki/crawl", body)
		if code != http.StatusAccepted {
			t.Fatalf("crawl: %d %s", code, data)
		}
		svc.Runner.Wait()
		var run crawler.CrawlRun
		db.First(&run, parse[crawler.CrawlRun](t, data).Data.ID)
		return run, run.Stats.Data()
	}
	count := func(where string) (n int64) {
		q := db.Model(&momuziki.Song{})
		if where != "" {
			q = q.Where(where)
		}
		q.Count(&n)
		return
	}
	cursor := func() map[string]any {
		_, data := call(http.MethodGet, "/api/v1/momuziki/status", "")
		return parse[map[string]any](t, data).Data["cursor"].(map[string]any)
	}

	// A. Page budget: category 8 pages 1-2, resumable at page 3.
	run, a := crawl(`{"maxPages":2,"maxDetails":0}`)
	if len(run.Errors) > 0 || count("") != 6 || a.DetailsFetched != 6 {
		t.Fatalf("A: songs=%d errors=%v stats=%+v", count(""), run.Errors, a)
	}
	if c := cursor(); c["phase"] != "categories" || c["categoryId"] != float64(8) || c["page"] != float64(3) {
		t.Fatalf("A: cursor %v", c)
	}
	var s momuziki.Song
	db.First(&s, 18)
	if s.Plays == nil || *s.Plays != 180 || s.ImageURL != srv.URL+"/music/images/18_big.jpeg" ||
		s.AudioURL != "https://momuziki.com/music/uploads/Artist%200%20-%20Song%2018%20%28Audio%29.mp3" || s.CategoryID != 8 {
		t.Fatalf("A: song 18 stored badly: %+v plays=%v", s, s.Plays)
	}

	// B. Finish categories, sweep ids 1..20 not listed, then probe above 20
	// until 3 misses in a row: finds 1, 7, 10, 16 and 21, 23.
	probesBefore := site.songProbe
	run, b := crawl(`{"maxProbes":0,"maxDetails":0}`)
	if len(run.Errors) > 0 || count("") != 20 || count("category_id = 0") != 6 || b.ProbeFound != 6 {
		t.Fatalf("B: songs=%d uncategorized=%d errors=%v stats=%+v", count(""), count("category_id = 0"), run.Errors, b)
	}
	// probes: 1,4,7,10,13,16 + 21,22,23,24,25,26; details: the 8 songs listed in B
	if b.Probed != 12 || site.songProbe-probesBefore != 12+8 {
		t.Fatalf("B: probed=%d song.php requests=%d", b.Probed, site.songProbe-probesBefore)
	}
	if c := cursor(); c["phase"] != "synced" {
		t.Fatalf("B: cursor %v", c)
	}

	// C. A new song at the top of category 8. Synced runs read each
	// category from page 1 and stop at the first page with nothing new;
	// missing ids are not re-probed; above-max probing costs ProbeAhead.
	site.mu.Lock()
	site.cats[8] = append([]uint{27}, site.cats[8]...)
	site.exists[27], site.plays[27] = true, 270
	site.mu.Unlock()
	hitsBefore, probesBefore := len(site.hits), site.songProbe
	run, cs := crawl(``)
	if len(run.Errors) > 0 || cs.SongsInserted != 1 || cs.PagesFetched != 1+2+1 || cs.DetailsFetched != 1 {
		t.Fatalf("C: errors=%v stats=%+v", run.Errors, cs)
	}
	if site.songProbe-probesBefore != 3+1 { // 28,29,30 ahead + song 27's page
		t.Fatalf("C: song.php requests=%d (hits map grew by %d)", site.songProbe-probesBefore, len(site.hits)-hitsBefore)
	}

	// D. Endpoints.
	_, data := call(http.MethodGet, "/api/v1/momuziki/categories", "")
	cats := parse[[]momuziki.Category](t, data)
	if len(cats.Data) != 2 || cats.Data[0].ID != 8 || cats.Data[0].Songs != 8 || cats.Meta["uncategorized"] != float64(6) {
		t.Fatalf("D: categories %s", data)
	}
	_, data = call(http.MethodGet, "/api/v1/momuziki/categories/22/songs?limit=5&sort=plays", "")
	songs := parse[[]momuziki.Song](t, data)
	if songs.Pagination.Total != 7 || len(songs.Data) != 5 || *songs.Data[0].Plays != 190 {
		t.Fatalf("D: category songs %s", data)
	}
	code, data := call(http.MethodGet, "/api/v1/momuziki/search?q="+url.QueryEscape("song 1"), "")
	found := parse[[]momuziki.Song](t, data)
	if code != 200 || found.Meta["source"] != "momuziki.com" || len(found.Data) != 10 || found.Data[0].ID != 1 || found.Data[0].Plays == nil {
		t.Fatalf("D: live search %d %s", code, data)
	}
	if site.hits["/search.php?query=song+1"] != 1 {
		t.Fatalf("D: search URL: %v", site.hits)
	}

	// E. Blocked: the guard stops after 3 challenges, pauses the crawler,
	// and search falls back to the local catalog.
	site.mu.Lock()
	site.blockAll = true
	site.mu.Unlock()
	run, _ = crawl(`{"refreshCategories":true}`)
	if run.Status != crawler.StatusFailed || !strings.Contains(strings.Join(run.Errors, " "), "paused until") {
		t.Fatalf("E: %s %v", run.Status, run.Errors)
	}
	if code, _ := call(http.MethodPost, "/api/v1/momuziki/crawl", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("E: crawl while paused: %d", code)
	}
	code, data = call(http.MethodGet, "/api/v1/momuziki/search?q=artist+2", "")
	local := parse[[]momuziki.Song](t, data)
	if code != 200 || local.Meta["source"] != "local" || local.Meta["fallback"] == nil || len(local.Data) == 0 {
		t.Fatalf("E: search fallback %d %s", code, data)
	}
	if code, _ := call(http.MethodPost, "/api/v1/momuziki/guard/reset", ""); code != 200 {
		t.Fatal("E: reset")
	}
}
