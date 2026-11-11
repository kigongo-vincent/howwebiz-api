package mzikii

import (
	"errors"
	"os"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

func TestParseSongPage(t *testing.T) {
	body, err := os.ReadFile("testdata/song_70.html")
	if err != nil {
		t.Fatal(err)
	}
	site := Site{Base: "https://www.mzikii.com"}
	tracks, err := site.ParseSongPage(body)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks=%v err=%v", tracks, err)
	}
	tr := tracks[0]
	if tr.ExternalID != "70" || tr.Title != "Byenyenya" || tr.Artist != "Fik Fameica" || tr.Genre != "" ||
		tr.AudioURL != "https://mzikii.com/music/uploads/63842_[Mp3jaja.com-Music]-Byenyenya---Fik-Fameica.mp3" ||
		tr.CoverURL != "https://mzikii.com/music/images/23892_mp3_thumb.jpg" ||
		tr.PageURL != "https://www.mzikii.com/songs.php?song_id=70" {
		t.Fatalf("%+v", tr)
	}
	if !site.IsSongPage("/songs.php") || site.IsSongPage("/single_artist.php") {
		t.Error("IsSongPage")
	}
	if _, err := site.ParseSongPage([]byte("<html><body>gone</body></html>")); !errors.Is(err, sitecrawl.ErrNotFound) {
		t.Errorf("empty page: %v", err)
	}
}
