package test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/audiocom"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
)

// fakeAudioCom implements the parts of api.audio.com the source uses:
// RFC 8414 discovery, dynamic client registration, authorization code +
// PKCE (S256), refresh-token rotation and GET /v1/search.
type fakeAudioCom struct {
	URL string

	mu         sync.Mutex
	tracks     []map[string]any // newest first
	challenge  string
	valid      map[string]bool // access tokens accepted
	refresh    string
	refreshes  int
	searches   atomic.Int64
	throttleN  atomic.Int64 // next N searches get 429
	registered bool
}

func newFakeAudioCom(n int) *fakeAudioCom {
	f := &fakeAudioCom{valid: map[string]bool{}}
	for i := n; i >= 1; i-- {
		f.tracks = append(f.tracks, fakeTrack(i))
	}
	return f
}

func fakeTrack(i int) map[string]any {
	tag := "rock"
	if i%2 == 1 {
		tag = "pop"
	}
	return map[string]any{
		"id": fmt.Sprintf("a%d", i), "title": fmt.Sprintf("Track %d", i), "slug": fmt.Sprintf("track-%d", i),
		"username": "artist", "author_name": "The Artist", "category": "music", "license": "cc-by",
		"tags": []string{tag}, "image": "", "is_listed": true, "is_downloadable": false,
		"date_created": 1700000000 + i, "date_updated": 1700000000 + i,
		"play": map[string]any{"url": fmt.Sprintf("https://s3.audio.com/audio/a%d.mp3", i), "duration": 1000 * i, "waveform": ""},
	}
}

func (f *fakeAudioCom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		enc.Encode(map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/auth/authorize",
			"token_endpoint": f.URL + "/auth/token", "registration_endpoint": f.URL + "/auth/client",
		})
	case "/auth/client":
		f.registered = true
		enc.Encode(map[string]any{"client_id": "cid"})
	case "/auth/token":
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("client_id") != "cid" {
				w.WriteHeader(400)
				enc.Encode(map[string]any{"error": "invalid_grant"})
				return
			}
			f.valid["at1"], f.refresh = true, "rt1"
			enc.Encode(map[string]any{"access_token": "at1", "refresh_token": "rt1", "expires_in": 3600})
		case "refresh_token":
			if r.Form.Get("refresh_token") != f.refresh {
				w.WriteHeader(400)
				enc.Encode(map[string]any{"error": "invalid_grant"})
				return
			}
			f.refreshes++
			at, rt := fmt.Sprintf("at%d", f.refreshes+1), fmt.Sprintf("rt%d", f.refreshes+1)
			f.valid[at], f.refresh = true, rt
			enc.Encode(map[string]any{"access_token": at, "refresh_token": rt, "expires_in": 3600})
		}
	case "/v1/search":
		f.searches.Add(1)
		if !f.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			w.WriteHeader(401)
			enc.Encode(map[string]any{"name": "Unauthorized", "message": "invalid credentials", "status": 401})
			return
		}
		if f.throttleN.Load() > 0 {
			f.throttleN.Add(-1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		query := strings.ToLower(q.Get("query"))
		var hits []map[string]any
		for _, t := range f.tracks {
			tags := t["tags"].([]string)
			switch {
			case query == "":
				hits = append(hits, t)
			case strings.HasPrefix(query, "#"):
				if tags[0] == query[1:] {
					hits = append(hits, t)
				}
			case strings.Contains(strings.ToLower(t["title"].(string)), query):
				hits = append(hits, t)
			}
		}
		out := []map[string]any{}
		for i := (page - 1) * 10; i < min(page*10, len(hits)); i++ {
			out = append(out, map[string]any{"type": "audio", "model": hits[i]})
		}
		if q.Get("types") == "" && page == 1 && query != "" {
			out = append(out, map[string]any{"type": "author", "model": map[string]any{"id": "u1", "username": "artist"}})
		}
		enc.Encode(out)
	default:
		w.WriteHeader(404)
	}
}

func TestAudioCom(t *testing.T) {
	fake := newFakeAudioCom(25) // a25 (newest) .. a1
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.URL = srv.URL

	prev := os.Getenv("DB_PATH")
	os.Setenv("DB_PATH", filepath.Join(t.TempDir(), "audiocom.db"))
	db := config.ConnectDB()
	os.Setenv("DB_PATH", prev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := audiocom.NewService(ctx, db, audiocom.Config{
		APIBase: srv.URL, RedirectURI: "http://localhost:3000/api/v1/audiocom/callback",
		RatePerMin: 600000, TagRefresh: 1 << 62, SearchTTL: 1 << 62,
	}, audiocom.WebConfigFromEnv())
	defer svc.Runner.Wait()
	app := config.SetupApplication()
	config.RegisterAppRoutes(config.Dependencies{DB: db, App: app, AudioCom: svc,
		Crawler: crawler.NewEngine(ctx, db, crawler.ConfigFromEnv())})

	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req := httptest.NewRequest(method, path, rd)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := app.Test(req, 10000)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, data
	}
	crawlNow := func(body any) crawler.Stats {
		t.Helper()
		code, data := call(http.MethodPost, "/api/v1/audiocom/crawl", body)
		if code != fiber.StatusAccepted {
			t.Fatalf("crawl: %d %s", code, data)
		}
		svc.Runner.Wait()
		var run crawler.CrawlRun
		db.First(&run, parse[crawler.CrawlRun](t, data).Data.ID)
		if len(run.Errors) > 0 {
			t.Fatalf("crawl errors: %v", run.Errors)
		}
		return run.Stats.Data()
	}

	// --- not connected yet ---
	if code, _ := call(http.MethodPost, "/api/v1/audiocom/crawl", nil); code != http.StatusPreconditionFailed {
		t.Fatalf("crawl before connect: want 412, got %d", code)
	}

	// --- connect: dynamic registration + PKCE ---
	code, data := call(http.MethodGet, "/api/v1/audiocom/connect", nil)
	if code != 200 || !fake.registered {
		t.Fatalf("connect: %d %s", code, data)
	}
	au, _ := url.Parse(parse[map[string]string](t, data).Data["authorizeUrl"])
	aq := au.Query()
	if au.Path != "/auth/authorize" || aq.Get("client_id") != "cid" || aq.Get("code_challenge_method") != "S256" ||
		aq.Get("scope") != "public" || aq.Get("redirect_uri") != "http://localhost:3000/api/v1/audiocom/callback" {
		t.Fatalf("authorize url: %s", au)
	}
	fake.mu.Lock()
	fake.challenge = aq.Get("code_challenge")
	fake.mu.Unlock()
	if code, _ := call(http.MethodGet, "/api/v1/audiocom/callback?code=the-code&state=wrong", nil); code != 400 {
		t.Fatalf("callback with bad state: want 400, got %d", code)
	}
	if code, data := call(http.MethodGet, "/api/v1/audiocom/callback?code=the-code&state="+aq.Get("state"), nil); code != 200 {
		t.Fatalf("callback: %d %s", code, data)
	}
	var a audiocom.Auth
	db.First(&a)
	if a.AccessToken != "at1" || a.RefreshToken != "rt1" || a.PendingVerifier != "" {
		t.Fatalf("tokens not stored: %+v", a)
	}

	// --- full crawl: "all" newest-first + "#pop"; one 429 is retried ---
	fake.throttleN.Store(1)
	s1 := crawlNow(map[string]any{"tags": []string{"pop"}})
	var n int64
	db.Model(&audiocom.Track{}).Count(&n)
	// all: pages 1-3 (10+10+5) + empty p4; #pop: 13 tracks, p1-2 + empty p3; +1 throttled
	if n != 25 || s1.SongsInserted != 25 || s1.SongsUnchanged != 13 || s1.PagesFetched != 7 || fake.searches.Load() != 8 {
		t.Fatalf("crawl 1: tracks=%d searches=%d stats=%+v", n, fake.searches.Load(), s1)
	}
	var tr audiocom.Track
	db.First(&tr, "id = ?", "a7")
	if tr.PlayURL != "https://s3.audio.com/audio/a7.mp3" || tr.DurationMs != 7000 || tr.Tags[0] != "pop" {
		t.Fatalf("track stored badly: %+v", tr)
	}

	// --- incremental: 2 new tracks; stops at the first page with nothing
	// new; #pop was walked recently so it is skipped ---
	fake.mu.Lock()
	fake.tracks = append([]map[string]any{fakeTrack(27), fakeTrack(26)}, fake.tracks...)
	fake.mu.Unlock()
	before := fake.searches.Load()
	s2 := crawlNow(map[string]any{"tags": []string{"pop"}})
	if s2.SongsInserted != 2 || fake.searches.Load()-before != 2 {
		t.Fatalf("crawl 2: searches=%d stats=%+v", fake.searches.Load()-before, s2)
	}

	// --- expired token → refreshed once, transparently ---
	fake.mu.Lock()
	fake.valid["at1"] = false
	fake.mu.Unlock()
	// "track 1" matches Track 1 and Track 10-19: page 1 = 10 audio + 1 author
	code, data = call(http.MethodGet, "/api/v1/audiocom/search?q=track%201", nil)
	if code != 200 {
		t.Fatalf("search: %d %s", code, data)
	}
	out := parse[[]audiocom.SearchResult](t, data)
	if fake.refreshes != 1 || len(out.Data) != 11 || out.Data[len(out.Data)-1].Type != "author" || out.Meta["cached"] != false {
		t.Fatalf("search after expiry: refreshes=%d results=%d meta=%v", fake.refreshes, len(out.Data), out.Meta)
	}
	db.First(&a)
	if a.RefreshToken != "rt2" {
		t.Fatalf("rotated refresh token not stored: %q", a.RefreshToken)
	}
	before = fake.searches.Load()
	if _, data := call(http.MethodGet, "/api/v1/audiocom/search?q=Track%201", nil); parse[[]audiocom.SearchResult](t, data).Meta["cached"] != true || fake.searches.Load() != before {
		t.Fatal("repeat search should be served from cache")
	}

	// --- stored tracks ---
	_, data = call(http.MethodGet, "/api/v1/audiocom/tracks?tag=pop&limit=5", nil)
	tracks := parse[[]audiocom.Track](t, data)
	if tracks.Pagination.Total != 14 || len(tracks.Data) != 5 || tracks.Data[0].ID != "a27" {
		t.Fatalf("tracks by tag: total=%d first=%+v", tracks.Pagination.Total, tracks.Data)
	}
	if code, _ := call(http.MethodGet, "/api/v1/audiocom/tracks/a3", nil); code != 200 {
		t.Fatalf("track a3: %d", code)
	}
	if code, _ := call(http.MethodGet, "/api/v1/audiocom/tracks/nope", nil); code != 404 {
		t.Fatalf("missing track: %d", code)
	}

	// --- disconnect ---
	if code, _ := call(http.MethodPost, "/api/v1/audiocom/disconnect", nil); code != 200 {
		t.Fatal("disconnect failed")
	}
	if code, _ := call(http.MethodPost, "/api/v1/audiocom/crawl", nil); code != http.StatusPreconditionFailed {
		t.Fatalf("crawl after disconnect: want 412, got %d", code)
	}
}
