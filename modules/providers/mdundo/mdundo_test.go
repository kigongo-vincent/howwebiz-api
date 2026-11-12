package mdundo

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

var site = Site{Base: "https://play.mdundo.com"}

func fixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseChart(t *testing.T) {
	tracks, err := site.parseCards(fixture(t, "top_charts_ug.html"), "/top-charts/ug")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 8 {
		t.Fatalf("got %d tracks, want 8", len(tracks))
	}
	first := tracks[0]
	if first.ExternalID != "6668647" || first.Title != "Nigina" || first.Artist != "Hoozambe D.Star" ||
		first.AudioURL != "https://mdundo.com/stream/6668647" || first.DurationMs != 147000 ||
		!strings.HasPrefix(first.CoverURL, "https://mdundo.com/media/picture/") ||
		first.PageURL != "https://play.mdundo.com/artist/659108/Hoozambe-D-Star#autoplay-6668647" || first.Kind != "" {
		t.Fatalf("first: %+v", first)
	}
	for _, tr := range tracks {
		if tr.Title == "" || tr.Artist == "" || !strings.HasPrefix(tr.AudioURL, "https://mdundo.com/stream/") {
			t.Errorf("incomplete: %+v", tr)
		}
	}
}

func TestParseSearchStripsBy(t *testing.T) {
	tracks, err := site.ParseSearch(fixture(t, "search_kenzo.html"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tr := range tracks {
		if tr.ExternalID == "2932247" {
			found = true
			if tr.Title != "Balippila Boda" || tr.Artist != "Eddy Kenzo" {
				t.Fatalf("2932247: %q by %q", tr.Title, tr.Artist)
			}
		}
	}
	if !found || len(tracks) == 0 {
		t.Fatalf("search: %d tracks, Balippila Boda found=%v", len(tracks), found)
	}
}

func TestDJMixesAreMixes(t *testing.T) {
	get := func(_ context.Context, path string) ([]byte, error) {
		if path != "/genre/245/DJ-Mixes" {
			t.Fatalf("fetched %q", path)
		}
		return fixture(t, "genre_dj_mixes.html"), nil
	}
	tracks, _, done, err := site.FeedPage(context.Background(), get, "genre/245/DJ-Mixes", "")
	if err != nil || !done || len(tracks) == 0 {
		t.Fatalf("tracks=%d done=%v err=%v", len(tracks), done, err)
	}
	for _, tr := range tracks {
		if tr.Kind != catalog.KindMix || tr.Genre != "DJ Mixes" {
			t.Errorf("not a mix: %+v", tr)
		}
	}
}

func TestSkipsPremium(t *testing.T) {
	body := strings.Replace(string(fixture(t, "top_charts_ug.html")), `data-is-premium="0"`, `data-is-premium="1"`, 1)
	tracks, _ := site.parseCards([]byte(body), "/top-charts/ug")
	if len(tracks) != 7 || tracks[0].ExternalID == "6668647" {
		t.Fatalf("premium track kept: %d tracks", len(tracks))
	}
}

func TestHelpers(t *testing.T) {
	if got := clean("Dance By Kado Ovic &amp;amp; Hustle Life"); got != "Dance By Kado Ovic & Hustle Life" {
		t.Errorf("clean: %q", got)
	}
	if got := stripBy("Mwana By Bebe Cool", "Bebe Cool"); got != "Mwana" {
		t.Errorf("stripBy: %q", got)
	}
	if got := stripBy("Stand By Me", "Ben E. King"); got != "Stand By Me" {
		t.Errorf("stripBy kept title: %q", got)
	}
	if genreOf("genre/9/Rhumba-Zilizopendwa") != "Rhumba Zilizopendwa" || genreOf("top-charts/ug") != "" {
		t.Error("genreOf")
	}
}

// The chart widget sits on every page: on the DJ-Mixes page its songs are
// still songs, without the page's genre.
func TestOtherListsKeepTheirOwnKind(t *testing.T) {
	page := string(fixture(t, "genre_dj_mixes.html")) + string(fixture(t, "top_charts_ug.html"))
	tracks, err := site.parseCards([]byte(page), "/genre/245/DJ-Mixes")
	if err != nil {
		t.Fatal(err)
	}
	var mixes, songs int
	for _, tr := range tracks {
		switch {
		case tr.ExternalID == "6668647": // "Nigina", the Uganda chart's #1
			if tr.Kind != "" || tr.Genre != "" {
				t.Errorf("chart song tagged: %+v", tr)
			}
			songs++
		case tr.Kind == catalog.KindMix && tr.Genre == "DJ Mixes":
			mixes++
		}
	}
	if mixes != 3 || songs != 1 {
		t.Fatalf("mixes=%d chart songs=%d", mixes, songs)
	}
}

// Every artist the charts found gets their whole catalog read, one page
// at a time, resuming where the last run stopped as the list grows.
func TestArtistsFeed(t *testing.T) {
	artists := []string{"/artist/360112/Eddy-Kenzo"}
	s := Site{Base: "https://play.mdundo.com", Feed: []string{"top-charts/ng"},
		Artists: func() ([]string, error) { return artists, nil }}
	if f := s.Feeds(); len(f) != 2 || f[1] != ArtistsFeed || !s.Tail(ArtistsFeed) || s.Tail("top-charts/ng") {
		t.Fatalf("feeds %v", f)
	}
	var fetched []string
	get := func(_ context.Context, p string) ([]byte, error) {
		fetched = append(fetched, p)
		return fixture(t, "artist_eddy_kenzo.html"), nil
	}
	tracks, next, done, err := s.FeedPage(context.Background(), get, ArtistsFeed, "")
	if err != nil || next != "1" || !done || len(tracks) == 0 || fetched[0] != "/artist/360112/Eddy-Kenzo" {
		t.Fatalf("tracks=%d next=%q done=%v err=%v fetched=%v", len(tracks), next, done, err, fetched)
	}
	// caught up: nothing fetched, cursor kept
	if _, next, done, _ := s.FeedPage(context.Background(), get, ArtistsFeed, "1"); next != "1" || !done || len(fetched) != 1 {
		t.Fatalf("caught up: next=%q done=%v fetched=%d", next, done, len(fetched))
	}
	// a new artist found later is read on the next run, from the stored cursor
	artists = append(artists, "/artist/659108/Hoozambe-D-Star")
	if _, next, _, _ := s.FeedPage(context.Background(), get, ArtistsFeed, "1"); next != "2" || fetched[1] != "/artist/659108/Hoozambe-D-Star" {
		t.Fatalf("new artist: next=%q fetched=%v", next, fetched)
	}
	if (Site{}).Feeds()[len(DefaultFeeds)-1] != DefaultFeeds[len(DefaultFeeds)-1] || len((Site{}).Feeds()) != len(DefaultFeeds) {
		t.Error("no Artists func: no artists feed")
	}
}

func TestArtistPath(t *testing.T) {
	if p := ArtistPath("https://play.mdundo.com/artist/360112/Eddy-Kenzo#autoplay-2932247"); p != "/artist/360112/Eddy-Kenzo" {
		t.Errorf("%q", p)
	}
	if ArtistPath("https://example.com/song/1") != "" {
		t.Error("non-artist url")
	}
}
