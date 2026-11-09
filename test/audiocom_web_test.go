package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/audiocom"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

// fakeAudioWeb is audio.com's website as a crawler might see it.
type fakeAudioWeb struct {
	mu       sync.Mutex
	genres   map[string]int // genre → number of tracks (10 per page)
	order    []string
	hits     map[string]int // path?query → requests
	blockAll bool
	blockN   int  // block the next N requests
	noData   bool // 200 pages without embedded data
	cookies  atomic.Int64
	agents   map[string]bool
}

const challengeHTML = `<!doctype html><html><head><title>Just a moment...</title></head><body>challenge-platform</body></html>`

func rscGenrePage(title string, ids []string, links ...string) string {
	var objs []map[string]any
	for _, id := range ids {
		objs = append(objs, map[string]any{"id": id, "title": "Track " + id, "slug": id, "username": "artist",
			"author_name": "Artist", "tags": []string{title}, "play": map[string]any{"url": "https://s3.audio.com/" + id + ".mp3", "duration": 1000}})
	}
	payload, _ := json.Marshal([]any{"$", "div", nil, map[string]any{"items": objs}})
	flight, _ := json.Marshal("5:" + string(payload) + "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "<html><head><title>%s</title></head><body>", title)
	for _, l := range links {
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, l, l)
	}
	fmt.Fprintf(&b, "<script>self.__next_f.push([1,%s])</script></body></html>", flight)
	return b.String()
}

func (f *fakeAudioWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Cookie") != "" {
		f.cookies.Add(1)
	}
	f.agents[r.UserAgent()] = true
	http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "should-never-be-sent-back", Path: "/"})

	if r.URL.Path == "/robots.txt" {
		w.Write([]byte("User-Agent: *\nAllow: /search\nAllow: /*?page=\nDisallow: *?*\nDisallow: /genres/secret\n"))
		return
	}
	key := r.URL.Path
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	f.hits[key]++
	if f.blockAll || f.blockN > 0 {
		if f.blockN > 0 {
			f.blockN--
		}
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(challengeHTML))
		return
	}
	switch {
	case r.URL.Path == "/genres":
		var links []string
		for _, g := range f.order {
			links = append(links, "/genres/"+g)
		}
		w.Write([]byte(rscGenrePage("Genres", nil, links...)))
	case strings.HasPrefix(r.URL.Path, "/genres/"):
		g := strings.TrimPrefix(r.URL.Path, "/genres/")
		n, ok := f.genres[g]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if f.noData {
			w.Write([]byte("<html><head><title>" + g + "</title></head><body>no data here</body></html>"))
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		var ids []string
		for i := (page-1)*10 + 1; i <= min(page*10, n); i++ {
			ids = append(ids, fmt.Sprintf("%s-%d", g, i))
		}
		w.Write([]byte(rscGenrePage(g, ids)))
	case r.URL.Path == "/search":
		w.Write([]byte(rscGenrePage("search", []string{"bnxn-1", "bnxn-2"})))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAudioWeb) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.hits {
		n += v
	}
	return n
}

func TestAudioComWebGuards(t *testing.T) {
	site := &fakeAudioWeb{
		genres: map[string]int{"pop": 25, "rock": 5, "secret": 3},
		order:  []string{"pop", "rock", "secret"},
		hits:   map[string]int{}, agents: map[string]bool{},
	}
	srv := httptest.NewServer(site)
	defer srv.Close()

	prev := os.Getenv("DB_PATH")
	os.Setenv("DB_PATH", filepath.Join(t.TempDir(), "web.db"))
	db := config.ConnectDB()
	os.Setenv("DB_PATH", prev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := audiocom.NewService(ctx, db, audiocom.Config{APIBase: srv.URL, RatePerMin: 600000, TagRefresh: time.Hour, SearchTTL: time.Hour},
		audiocom.WebConfig{Config: webguard.Config{BaseURL: srv.URL, UserAgent: "howwebiz-test/1.0", Delay: 0,
			MaxConsecutive: 3, MaxPerRun: 5, Backoff: 5 * time.Millisecond,
			Cooldown: time.Hour, MaxCooldown: 4 * time.Hour}, MaxUnrecognized: 2, GenreRefresh: time.Hour})
	defer svc.Runner.Wait()
	app := config.SetupApplication()
	config.RegisterAppRoutes(config.Dependencies{DB: db, App: app, AudioCom: svc,
		Crawler: crawler.NewEngine(ctx, db, crawler.ConfigFromEnv())})

	call := func(method, path string, body string) (int, []byte) {
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
	crawl := func(body string) crawler.CrawlRun {
		t.Helper()
		code, data := call(http.MethodPost, "/api/v1/audiocom/web/crawl", body)
		if code != http.StatusAccepted {
			t.Fatalf("web crawl: %d %s", code, data)
		}
		svc.Runner.Wait()
		var run crawler.CrawlRun
		db.First(&run, parse[crawler.CrawlRun](t, data).Data.ID)
		return run
	}
	health := func() webguard.Health { h, _ := svc.Web.Health(); return h }
	tracks := func() (n int64) { db.Model(&audiocom.Track{}).Count(&n); return }

	// A. Normal crawl: every genre to its last page; robots.txt keeps the
	// crawler out of /genres/secret; no cookie is ever sent back.
	run := crawl(``)
	st := run.Stats.Data()
	if tracks() != 30 || st.SongsInserted != 30 || site.hits["/genres/secret"] != 0 {
		t.Fatalf("A: tracks=%d stats=%+v secretHits=%d", tracks(), st, site.hits["/genres/secret"])
	}
	if run.Status != crawler.StatusPartial || len(run.Errors) != 1 || !strings.Contains(run.Errors[0], "robots.txt") {
		t.Fatalf("A: want the disallowed genre reported, got %s %v", run.Status, run.Errors)
	}
	// pop has 25 tracks: p1, p2 full, p3 has 5, p4 is empty → the walk ends at p4
	if site.hits["/genres/pop?page=4"] != 1 || site.hits["/genres/pop?page=5"] != 0 {
		t.Fatalf("A: pagination hits %v", site.hits)
	}
	if site.cookies.Load() != 0 || len(site.agents) != 1 || !site.agents["howwebiz-test/1.0"] {
		t.Fatalf("A: cookies sent=%d agents=%v", site.cookies.Load(), site.agents)
	}

	// B. Intermittent block: two challenges, then the page loads. The
	// crawler backs off and retries the same URL; the streak resets.
	site.mu.Lock()
	site.blockN = 2
	site.mu.Unlock()
	run = crawl(`{"genres":["rock"],"reset":true}`)
	h := health()
	if run.Status != crawler.StatusSucceeded || h.TotalBlocks != 2 || h.ConsecutiveBlocks != 0 || h.BlockedUntil != nil || h.LastBlockReason == "" {
		t.Fatalf("B: status=%s errors=%v health=%+v", run.Status, run.Errors, h)
	}

	// C. Persistent block: after 3 in a row the circuit opens, the run
	// stops, and nothing else is requested until the cooldown passes.
	site.mu.Lock()
	site.blockAll = true
	site.mu.Unlock()
	before := site.requests()
	run = crawl(`{"reset":true}`)
	h = health()
	if site.requests()-before != 3 || h.Trips != 1 || h.ConsecutiveBlocks != 3 || h.BlockedUntil == nil {
		t.Fatalf("C: requests=%d health=%+v", site.requests()-before, h)
	}
	if d := time.Until(*h.BlockedUntil); d < 59*time.Minute || d > time.Hour {
		t.Fatalf("C: cooldown %v, want ~1h", d)
	}
	if run.Status != crawler.StatusFailed || !strings.Contains(strings.Join(run.Errors, " "), "paused until") {
		t.Fatalf("C: status=%s errors=%v", run.Status, run.Errors)
	}
	before = site.requests()
	if code, _ := call(http.MethodPost, "/api/v1/audiocom/web/crawl", ``); code != http.StatusServiceUnavailable {
		t.Fatalf("C: crawl while paused: want 503, got %d", code)
	}
	if code, _ := call(http.MethodGet, "/api/v1/audiocom/web/search?q=bnxn", ``); code != http.StatusServiceUnavailable {
		t.Fatalf("C: search while paused: want 503, got %d", code)
	}
	if site.requests() != before {
		t.Fatal("C: requests were sent while the circuit was open")
	}
	_, data := call(http.MethodGet, "/api/v1/audiocom/web/status", ``)
	if s := parse[map[string]any](t, data).Data; s["paused"] != true {
		t.Fatalf("C: status should report paused: %v", s)
	}

	// D. Operator reset while still blocked: it trips again and the
	// cooldown doubles.
	if code, _ := call(http.MethodPost, "/api/v1/audiocom/web/reset", ``); code != 200 {
		t.Fatal("D: reset failed")
	}
	crawl(`{"reset":true}`)
	h = health()
	if h.Trips != 2 || time.Until(*h.BlockedUntil) < 119*time.Minute {
		t.Fatalf("D: want 2nd trip with ~2h cooldown, got %+v", h)
	}

	// E. Pages load but carry no recognizable data: stop after 2 such
	// pages instead of walking everything for nothing.
	site.mu.Lock()
	site.blockAll, site.noData = false, true
	site.mu.Unlock()
	call(http.MethodPost, "/api/v1/audiocom/web/reset", ``)
	before = site.requests()
	run = crawl(`{"reset":true}`)
	if !strings.Contains(strings.Join(run.Errors, " "), "without recognizable data") || site.requests()-before != 3 { // /genres + pop + rock
		t.Fatalf("E: requests=%d errors=%v", site.requests()-before, run.Errors)
	}

	// F. Website search.
	site.mu.Lock()
	site.noData = false
	site.mu.Unlock()
	code, data := call(http.MethodGet, "/api/v1/audiocom/web/search?q=bnxn", ``)
	if code != 200 {
		t.Fatalf("F: search %d %s", code, data)
	}
	if out := parse[[]audiocom.Track](t, data); len(out.Data) != 2 || out.Data[0].PlayURL == "" {
		t.Fatalf("F: %s", data)
	}
	if site.hits["/search?search_text=bnxn&sort=relevance"] != 1 {
		t.Fatalf("F: search URL not as expected: %v", site.hits)
	}
}
