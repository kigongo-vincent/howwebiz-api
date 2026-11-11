package ugmuziki

import (
	"errors"
	"os"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

func TestParseSongPage(t *testing.T) {
	body, err := os.ReadFile("testdata/song.html")
	if err != nil {
		t.Fatal(err)
	}
	site := Site{Base: "https://www.ugmuziki.com"}
	tracks, err := site.ParseSongPage(body)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks=%v err=%v", tracks, err)
	}
	tr := tracks[0]
	if tr.ExternalID != "118" || tr.Title != "Tuliyambala Engule" || tr.Artist != "Bobi Wine Ft All Stars" ||
		tr.AudioURL != "https://www.ugmuziki.com/public/uploads/songs/Bobi%20Wine%20Ft.%20Allstars%20-%20Tuliyambala%20Engule-ugmuziki.mp3" ||
		tr.CoverURL != "https://www.ugmuziki.com/public/uploads/song_covers/ugmuziki-cover118.jpg" ||
		tr.PageURL != "https://www.ugmuziki.com/song/tuliyambala-engule-by-bobi-wine-ft-all-stars" ||
		tr.Plays == nil || *tr.Plays != 256 {
		t.Fatalf("%+v plays=%v", tr, tr.Plays)
	}
	if !site.IsSongPage("/song/abc") || site.IsSongPage("/blog/abc") || site.IsSongPage("/song/") {
		t.Error("IsSongPage")
	}
	if _, err := site.ParseSongPage([]byte("<html></html>")); !errors.Is(err, sitecrawl.ErrNotFound) {
		t.Errorf("empty: %v", err)
	}
}

// Most pages name the file "ugmuziki.com - Title By Artist - {id}.mp3": the
// site's name must never become the artist.
func TestParseSongPageSitePrefixedFile(t *testing.T) {
	body, err := os.ReadFile("testdata/song_site_prefix.html")
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := Site{Base: "https://www.ugmuziki.com"}.ParseSongPage(body)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks=%v err=%v", tracks, err)
	}
	if tr := tracks[0]; tr.ExternalID != "1000" || tr.Title != "Abasooka" || tr.Artist != "Terra Watts Ft Victor Ruz" {
		t.Fatalf("%q by %q (id %s)", tr.Title, tr.Artist, tr.ExternalID)
	}
	if m := byRe.FindStringSubmatch("Stand By Me By Ben E King"); m == nil || m[1] != "Stand By Me" || m[2] != "Ben E King" {
		t.Errorf("last By: %v", m)
	}
}
