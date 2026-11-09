package test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/song"
)

// fakeGoogle mimics the Custom Search JSON API for howwe.ug's engine.
type fakeGoogle struct{ hits atomic.Int64 }

var Google = &fakeGoogle{}

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.hits.Add(1)
	q := r.URL.Query()
	if q.Get("key") != "test-key" || q.Get("cx") != "76ecd7f17cc7e4f1b" {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"bad key or cx"}}`))
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"searchInformation": map[string]any{"totalResults": "1234"},
		"items": []map[string]string{
			{"title": "Nassanga (Remix) by Kalifah AgaNaga - MP3 Download, Audio Download - Howwe.ug",
				"link": "https://www.howwe.ug/song/126112/nassanga-remix/93/kalifah-aganaga", "snippet": "..."},
			{"title": "New Tune by Someone New - MP3 Download, Audio Download - Howwe.ug",
				"link": "https://www.howwe.ug/song/777777/new-tune/555/someone-new", "snippet": "..."},
			{"title": "Kalifah AgaNaga - Howwe.ug", "link": "https://www.howwe.ug/KalifahAgaNaga", "snippet": "..."},
			{"title": "Some story", "link": "https://www.howwe.ug/news/entertainment/1/x", "snippet": "..."},
		},
	})
}

type hit struct {
	Kind   string     `json:"kind"`
	SongID *uint      `json:"songId"`
	Song   *song.Song `json:"song"`
}

func TestSearchHowwe(t *testing.T) {
	// make sure 126112 is in the catalog
	crawl(t, map[string]any{"targets": []string{"hot100"}, "pages": 1, "fetchDetails": false})

	before := Google.hits.Load()
	res, data := do(t, http.MethodGet, BASE_URL("howwe/search?q=kalifah"), nil) // auto → howwe (key set)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("search: %d %s", res.StatusCode, data)
	}
	out := parse[[]hit](t, data)
	if out.Meta["source"] != "howwe" || out.Meta["cached"] != false || out.Pagination.Total != 100 || out.Pagination.Limit != 10 {
		t.Fatalf("meta/pagination: %+v %+v", out.Meta, out.Pagination)
	}
	kinds := []string{}
	for _, h := range out.Data {
		kinds = append(kinds, h.Kind)
	}
	if len(kinds) != 4 || kinds[0] != "song" || kinds[1] != "song" || kinds[2] != "artist" || kinds[3] != "news" {
		t.Fatalf("kinds: %v", kinds)
	}
	if out.Data[0].Song == nil || out.Data[0].Song.ID != 126112 {
		t.Fatalf("known song not joined: %+v", out.Data[0])
	}
	// The unknown hit was added to the catalog for the crawler to enrich.
	if s := out.Data[1].Song; s == nil || s.Title != "New Tune" || s.Artist != "Someone New" || s.ArtistID != 555 {
		t.Fatalf("new song not stored: %+v", out.Data[1])
	}

	// Same query again: served from cache, Google not called.
	_, data = do(t, http.MethodGet, BASE_URL("howwe/search?q=Kalifah"), nil)
	if parse[[]hit](t, data).Meta["cached"] != true || Google.hits.Load()-before != 1 {
		t.Fatalf("expected cached second query (google hits %d)", Google.hits.Load()-before)
	}

	// Google only serves the first 100 results.
	if res, _ := do(t, http.MethodGet, BASE_URL("howwe/search?q=kalifah&page=11"), nil); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("page 11: want 502, got %d", res.StatusCode)
	}
}

func TestSearchLocal(t *testing.T) {
	crawl(t, map[string]any{"targets": []string{"hot100"}, "pages": 1, "fetchDetails": false})
	get := func(q string) ([]song.Song, map[string]any) {
		t.Helper()
		res, data := do(t, http.MethodGet, BASE_URL("howwe/search?source=local&q="+url.QueryEscape(q)), nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%q: %d %s", q, res.StatusCode, data)
		}
		out := parse[[]song.Song](t, data)
		return out.Data, out.Meta
	}

	songs, meta := get("nassanga")
	if meta["source"] != "local" || len(songs) == 0 || songs[0].ID != 126112 {
		t.Fatalf("nassanga: %+v", songs)
	}
	// every word must match (title or artist), in any order
	if songs, _ := get("weasel nassanga"); len(songs) != 1 || songs[0].ID != 126112 {
		t.Fatalf("multi-word: %+v", songs)
	}
	if songs, _ := get("nassanga zzzz"); len(songs) != 0 {
		t.Fatalf("non-matching word should exclude: %+v", songs)
	}
	// LIKE wildcards are literal: "%" finds only titles containing "%"
	// (e.g. "Twingana 100%"), not every song.
	for _, w := range []string{"%", "_"} {
		songs, _ := get(w)
		for _, s := range songs {
			if !strings.Contains(s.Title+s.Artist, w) {
				t.Fatalf("%q matched %q / %q", w, s.Title, s.Artist)
			}
		}
	}
	if res, _ := do(t, http.MethodGet, BASE_URL("howwe/search?q="), nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty q: want 400, got %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, BASE_URL("howwe/search?q=x&source=bing"), nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad source: want 400, got %d", res.StatusCode)
	}
}
