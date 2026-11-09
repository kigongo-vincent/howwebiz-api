package test

import (
	"context"
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

	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
)

// catalogSite is a tiny howwe.ug: a newest-first listing paginated 3 per
// page (past the end it repeats the last page, like the real site) and
// song pages resolvable by id, some of which no listing shows.
type catalogSite struct {
	mu        sync.Mutex
	listed    []int            // /audio/new, newest first
	genres    map[string][]int // /genres/{name}, arbitrary order
	hidden    map[int]bool     // real songs absent from every listing
	probes    atomic.Int64     // requests to /song/{id}/x/1/x
	listHits  atomic.Int64     // /audio/new
	genreHits atomic.Int64     // /genres/{name}...
}

const perPage = 3

// grid renders one page of a song grid, clamping past the end like howwe.ug.
func grid(listPath string, ids []int, page int) string {
	last := max(1, (len(ids)+perPage-1)/perPage)
	page = min(page, last)
	var b strings.Builder
	b.WriteString(`<html><body><ul class="grid">`)
	for _, id := range ids[min((page-1)*perPage, len(ids)):min(page*perPage, len(ids))] {
		fmt.Fprintf(&b, `<li><a href="/song/%d/song-%d/9/artist-9"><img src="https://cdn.howwe.ug/_sng_cvrs_/%d.webp"><h3>Song %d</h3><h4>Artist 9</h4></a></li>`, id, id, id, id)
	}
	b.WriteString(`</ul>`)
	if page < last {
		fmt.Fprintf(&b, `<a href="%s/page/%d">next</a>`, listPath, page+1)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

func pageOf(p string) int {
	if i := strings.LastIndex(p, "/page/"); i >= 0 {
		n, _ := strconv.Atoi(p[i+6:])
		return n
	}
	return 1
}

func (s *catalogSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/genres":
		var b strings.Builder
		b.WriteString(`<html><body><ul>`)
		for name, ids := range s.genres {
			fmt.Fprintf(&b, `<li><a href="genres/%s"><h3>%s</h3><h4>%d songs</h4></a></li>`, name, name, len(ids))
		}
		b.WriteString(`</ul></body></html>`)
		w.Write([]byte(b.String()))
	case strings.HasPrefix(p, "/genres/"):
		s.genreHits.Add(1)
		name := strings.Split(strings.TrimPrefix(p, "/genres/"), "/")[0]
		w.Write([]byte(grid("/genres/"+name, s.genres[name], pageOf(p))))
	case strings.HasPrefix(p, "/audio/new"):
		s.listHits.Add(1)
		w.Write([]byte(grid("/audio/new", s.listed, pageOf(p))))
	case strings.HasPrefix(p, "/song/"):
		parts := strings.Split(p, "/")
		id, _ := strconv.Atoi(parts[2])
		if strings.HasSuffix(p, "/x/1/x") {
			s.probes.Add(1)
		}
		exists := s.hidden[id]
		for _, l := range s.listed {
			exists = exists || l == id
		}
		for _, ids := range s.genres {
			for _, g := range ids {
				exists = exists || g == id
			}
		}
		if !exists { // howwe.ug: 200 with an empty shell
			w.Write([]byte(`<html><head><base href='/'></head><body><h1></h1></body></html>`))
			return
		}
		fmt.Fprintf(w, `<html><head><base href='/'></head><body><h1>Song %d</h1>
			<a href='/song/%d/song-%d/9/artist-9'>self</a>
			<ul class='sm2-playlist-bd'><li><a href='_fxs_/_upls_/_sngs_/song-%d.mp3?_=1'><b>Song %d</b> <span class='label'>3.0 MB</span></a></li></ul>
			<a href='/Artist9' class="group"><label>Name:</label> <span>Artist 9</span></a></body></html>`, id, id, id, id, id)
	default:
		http.NotFound(w, r)
	}
}

func newCatalogEngine(t *testing.T, site http.Handler) (*crawler.Engine, *gorm.DB) {
	t.Helper()
	srv := httptest.NewServer(site)
	t.Cleanup(srv.Close)
	prev := os.Getenv("DB_PATH")
	os.Setenv("DB_PATH", filepath.Join(t.TempDir(), "catalog.db"))
	db := config.ConnectDB()
	os.Setenv("DB_PATH", prev)

	cfg := crawler.ConfigFromEnv()
	cfg.BaseURL, cfg.DetailWorkers = srv.URL, 4
	ctx, cancel := context.WithCancel(context.Background())
	e := crawler.NewEngine(ctx, db, cfg)
	t.Cleanup(func() { cancel(); e.Wait() })
	return e, db
}

func runCatalog(t *testing.T, e *crawler.Engine, db *gorm.DB, opts crawler.CatalogOptions) crawler.Stats {
	t.Helper()
	run, err := e.StartCatalog("test", opts)
	if err != nil {
		t.Fatal(err)
	}
	e.Wait()
	var done crawler.CrawlRun
	db.First(&done, run.ID)
	if len(done.Errors) > 0 {
		t.Fatalf("run errors: %v", done.Errors)
	}
	return done.Stats.Data()
}

func TestCatalogCrawlsEverySong(t *testing.T) {
	site := &catalogSite{
		genres: map[string][]int{
			"Pop":   {95, 7, 88, 99, 60}, // 7 and 60: old songs /audio/new never shows
			"Rock":  {86, 87, 3, 100},
			"Empty": {},
		},
		hidden: map[int]bool{50: true}, // in no listing at all
	}
	for id := 100; id >= 86; id-- { // /audio/new: 15 songs, 5 pages
		site.listed = append(site.listed, id)
	}
	e, db := newCatalogEngine(t, site)
	count := func() int64 { var n int64; db.Model(&song.Song{}).Count(&n); return n }
	genreOf := func(id uint) string { var s song.Song; db.First(&s, id); return s.Genre }

	// A: budget of 2 pages → Empty (1 page) + Pop page 1; resumes at Pop p2.
	a := runCatalog(t, e, db, crawler.CatalogOptions{MaxPages: 2})
	cur, _ := crawler.LoadCursor(db)
	if count() != 3 || cur.Phase != crawler.PhaseGenres || cur.Genre != "Pop" || cur.GenrePage != 2 || cur.GenresTotal != 3 || a.DetailsFetched != 3 {
		t.Fatalf("A: songs=%d cursor=%+v stats=%+v", count(), cur, a)
	}
	if genreOf(7) != "Pop" {
		t.Fatalf("A: song 7 genre = %q", genreOf(7))
	}

	// B: genres finish (Pop p2, Rock p1-2) → 9 songs incl. old ids 3, 7, 60.
	// Then the sweep probes 40 of the ids no genre listed.
	b := runCatalog(t, e, db, crawler.CatalogOptions{MaxProbes: 40})
	cur, _ = crawler.LoadCursor(db)
	if cur.Phase != crawler.PhaseSweep || cur.GenreWalkDoneAt == nil || cur.SweepMaxID != 100 || b.GenresCompleted != 2 || b.Probed != 40 {
		t.Fatalf("B: cursor=%+v stats=%+v", cur, b)
	}
	if genreOf(3) != "Rock" || genreOf(60) != "Pop" {
		t.Fatalf("B: genres 3=%q 60=%q", genreOf(3), genreOf(60))
	}

	// C: sweep finishes: finds song 50 and the /audio/new-only songs.
	c := runCatalog(t, e, db, crawler.CatalogOptions{})
	cur, _ = crawler.LoadCursor(db)
	if cur.Phase != crawler.PhaseSynced || c.ProbeFound != 10 || count() != 19 {
		t.Fatalf("C: songs=%d cursor=%+v stats=%+v", count(), cur, c)
	}
	if site.probes.Load() != 91 { // every id in 1..100 not in a genre, once
		t.Fatalf("probes = %d, want 91", site.probes.Load())
	}
	var s50 song.Song
	db.First(&s50, 50)
	if s50.Slug != "song-50" || s50.ArtistID != 9 || !strings.HasSuffix(s50.AudioURL, "/_fxs_/_upls_/_sngs_/song-50.mp3?_=1") {
		t.Fatalf("probed song stored badly: %+v", s50)
	}
	var withAudio int64
	db.Model(&song.Song{}).Where("audio_url <> ''").Count(&withAudio)
	if withAudio != 19 {
		t.Fatalf("songs with audio = %d, want 19", withAudio)
	}

	// D: two new songs. Synced runs read /audio/new newest-first and stop at
	// the first page with nothing new; genres aren't due; no re-probing.
	site.mu.Lock()
	site.listed = append([]int{102, 101}, site.listed...)
	site.mu.Unlock()
	lh, gh, pr := site.listHits.Load(), site.genreHits.Load(), site.probes.Load()
	d := runCatalog(t, e, db, crawler.CatalogOptions{})
	cur, _ = crawler.LoadCursor(db)
	if d.SongsInserted != 2 || site.listHits.Load()-lh != 2 || site.genreHits.Load() != gh || site.probes.Load() != pr ||
		cur.Phase != crawler.PhaseSynced || cur.SweepMaxID != 102 || d.DetailsFetched != 2 {
		t.Fatalf("D: listHits=%d genreHits=%d probes=%d cursor=%+v stats=%+v",
			site.listHits.Load()-lh, site.genreHits.Load()-gh, site.probes.Load()-pr, cur, d)
	}

	// E: forced genre re-walk. 101 moved into Pop → genre recorded; the
	// unchanged genre pages cost requests but no writes.
	site.mu.Lock()
	site.genres["Pop"] = append(site.genres["Pop"], 101)
	site.mu.Unlock()
	ev := runCatalog(t, e, db, crawler.CatalogOptions{RefreshGenres: true})
	if genreOf(101) != "Pop" || ev.SongsInserted != 0 || ev.SongsUpdated != 1 || ev.GenresCompleted != 3 || ev.PagesUnchanged < 3 {
		t.Fatalf("E: genre(101)=%q stats=%+v", genreOf(101), ev)
	}

	// Genre endpoints on this catalog.
	app := config.SetupApplication()
	config.RegisterAppRoutes(config.Dependencies{DB: db, App: app, Crawler: e})
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/genres", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	type g struct {
		Name      string `json:"name"`
		SiteCount int    `json:"siteCount"`
		Songs     int64  `json:"songs"`
	}
	gs := parse[[]g](t, body).Data
	if len(gs) != 3 || gs[0].Name != "Pop" || gs[0].SiteCount != 6 || gs[0].Songs != 6 {
		t.Fatalf("genres: %+v", gs)
	}
	res, _ = app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/genres/Rock/songs?limit=2", nil))
	body, _ = io.ReadAll(res.Body)
	if out := parse[[]song.Song](t, body); out.Pagination.Total != 4 || len(out.Data) != 2 || out.Data[0].Genre != "Rock" {
		t.Fatalf("rock songs: %s", body)
	}
	if res, _ := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/genres/Nope/songs", nil)); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown genre: %d", res.StatusCode)
	}
}
