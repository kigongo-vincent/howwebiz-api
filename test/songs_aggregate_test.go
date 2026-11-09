package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

// serveFixture answers robots.txt and serves one saved page for any
// search URL.
func serveFixture(t *testing.T, searchPath, fixture string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Write([]byte("User-agent: *\nAllow: /\n"))
		case searchPath:
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
}

// The aggregated /songs merges providers' copies of the same song, using
// the real search pages each site returned for "kenzo".
func TestSongsAggregatesProviders(t *testing.T) {
	mom := serveFixture(t, "/search.php", "../modules/momuziki/testdata/search_kenzo.html")
	pt := serveFixture(t, "/search", "../modules/providers/pearltunes/testdata/search_kenzo.html")
	defer mom.Close()
	defer pt.Close()
	for k, v := range map[string]string{
		"MOMUZIKI_BASE_URL": mom.URL, "PEARLTUNES_BASE_URL": pt.URL,
		"MOMUZIKI_DELAY": "0s", "PEARLTUNES_DELAY": "0s",
		"DB_PATH": filepath.Join(t.TempDir(), "agg.db"),
	} {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := config.ConnectDB()
	app := config.SetupApplication()
	deps := config.Wire(ctx, db, app, false)
	config.RegisterAppRoutes(deps)
	defer deps.Wait()

	get := func(path string) (int, []byte) {
		t.Helper()
		res, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), 30000)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}

	code, body := get("/api/v1/songs?q=kenzo&limit=100")
	if code != 200 {
		t.Fatalf("songs: %d %s", code, body)
	}
	out := parse[[]catalog.Song](t, body)

	live := map[string]string{}
	for _, l := range out.Meta["live"].([]any) {
		m := l.(map[string]any)
		live[m["provider"].(string)] = m["status"].(string)
	}
	// howwe uses the fake Google from TestMain; audio.com is not connected;
	// ugamusic has no live search (its robots.txt disallows /search?).
	want := map[string]string{"momuziki": "ok", "pearltunes": "ok", "howwe": "ok", "audiocom": "skipped", "ugamusic": ""}
	for p, st := range want {
		if live[p] != st {
			t.Errorf("live[%s] = %q, want %q (all: %v)", p, live[p], st, live)
		}
	}

	var sources, merged int
	providers := map[string]bool{}
	for _, s := range out.Data {
		sources += len(s.Sources)
		if s.SourceCount > 1 {
			merged++
		}
		for _, p := range s.Providers {
			providers[p] = true
		}
		if !strings.Contains(strings.ToLower(s.Artist+" "+s.Title+" "+strings.Join(s.Featured, " ")), "kenzo") {
			t.Errorf("unrelated result: %q by %q", s.Title, s.Artist)
		}
	}
	t.Logf("%d entries from %d provider copies; %d entries merged across providers; providers %v",
		len(out.Data), sources, merged, providers)
	if merged == 0 || sources <= len(out.Data) {
		t.Fatal("expected some songs to be merged")
	}
	// pearltunes' search page carries no mp3 and this fake serves no song
	// pages to enrich from, so nothing of pearltunes' is stored: the
	// catalog holds playable songs only.
	if !providers["momuziki"] || providers["pearltunes"] {
		t.Errorf("providers: %v", providers)
	}
	var silent int64
	db.Table("catalog_sources").Where("TRIM(audio_url) = ''").Count(&silent)
	if silent != 0 {
		t.Fatalf("%d stored sources without audio", silent)
	}
	for _, s := range out.Data {
		if s.AudioURL == "" {
			t.Errorf("entry without audio: %q", s.Title)
		}
	}

	// One merged entry, with every copy, best provider first.
	var best catalog.Song
	for _, s := range out.Data {
		if s.SourceCount > best.SourceCount {
			best = s
		}
	}
	code, body = get("/api/v1/songs/" + strconv.FormatUint(uint64(best.ID), 10))
	one := parse[catalog.Song](t, body)
	if code != 200 || len(one.Data.Sources) != best.SourceCount || one.Data.AudioURL == "" {
		t.Fatalf("song %d: %d %s", best.ID, code, body)
	}
	t.Logf("most-merged: %q by %s — %v", one.Data.Title, one.Data.Artist, one.Data.Providers)

	// Filters, and live=false answers from the catalog only.
	_, body = get("/api/v1/songs?q=kenzo&provider=momuziki&live=false&limit=100")
	for _, s := range parse[[]catalog.Song](t, body).Data {
		if !strings.Contains(strings.Join(s.Providers, ","), "momuziki") {
			t.Errorf("provider filter leaked %v", s.Providers)
		}
	}
	if code, _ := get("/api/v1/songs?sort=bogus"); code != 400 {
		t.Errorf("bad sort: %d", code)
	}
	code, body = get("/api/v1/providers")
	if code != 200 || !strings.Contains(string(body), `"pearltunes"`) || !strings.Contains(string(body), `"ugamusic"`) {
		t.Fatalf("providers: %d %s", code, body)
	}
}
