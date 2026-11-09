package ugamusic

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

var site = Site{Base: "https://ugamusic.net"}

// ugamusic implements only what its robots.txt allows: no Lister (pagination
// disallowed), no Searchable (search disallowed).
var (
	_ sitecrawl.Site       = site
	_ sitecrawl.Sitemapper = site
)

func TestCapabilitiesFollowRobots(t *testing.T) {
	var s sitecrawl.Site = site
	if _, ok := s.(sitecrawl.Lister); ok {
		t.Error("ugamusic must not list: robots.txt disallows /*?*page=")
	}
	if _, ok := s.(sitecrawl.Searchable); ok {
		t.Error("ugamusic must not search: robots.txt disallows /search?")
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSongPage(t *testing.T) {
	tracks, err := site.ParseSongPage(fixture(t, "song_page.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) < 20 {
		t.Fatalf("got %d tracks, want the song + related + recommended (>=20)", len(tracks))
	}
	n := tracks[0]
	if n.ExternalID != "28525" || n.Title != "Nkutwaleko" || n.Artist != "Biswanka" || n.Genre != "Afro beats" ||
		n.AudioURL != "https://ugamusic.net/storage/song_url/nkutwaleko-by-biswanka.mp3" || n.DurationMs != 146000 ||
		n.PageURL != "https://ugamusic.net/songs/nkutwaleko-by-biswanka-download" ||
		n.CoverURL != "https://ugamusic.net/storage/image_cover/Nkutwaleko%20-%20Biswanka_1793565423.jpg" ||
		n.Plays == nil || n.ReleasedAt == nil {
		t.Fatalf("song: %+v", n)
	}
	seen := map[string]bool{}
	for _, tr := range tracks {
		if seen[tr.ExternalID] {
			t.Errorf("duplicate %s", tr.ExternalID)
		}
		seen[tr.ExternalID] = true
		if strings.Contains(tr.AudioURL, " ") || tr.Title == "" || tr.Artist == "" {
			t.Errorf("bad track: %+v", tr)
		}
	}
	if _, err := site.ParseSongPage([]byte(`<div id="app" data-page="{&quot;props&quot;:{}}"></div>`)); err != sitecrawl.ErrNotFound {
		t.Errorf("page without a song: %v", err)
	}
}

func TestSitemapPaths(t *testing.T) {
	var doc struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(fixture(t, "sitemap_songs.xml"), &doc); err != nil || len(doc.URLs) != 25 {
		t.Fatalf("sitemap: %d urls, %v", len(doc.URLs), err)
	}
	for _, u := range doc.URLs {
		path := strings.TrimPrefix(u.Loc, "https://ugamusic.net")
		if !site.IsSongPage(path) {
			t.Errorf("not recognized as a song page: %s", path)
		}
	}
	for _, p := range []string{"/songs", "/songs/", "/artists/x", "/songs/x/y"} {
		if site.IsSongPage(p) {
			t.Errorf("%s should not be a song page", p)
		}
	}
}
