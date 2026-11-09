package scraper

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseHot100(t *testing.T) {
	entries, err := ParseHot100(fixture(t, "hot100.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100 {
		t.Fatalf("got %d entries, want 100", len(entries))
	}
	for i, e := range entries {
		if e.Position != i+1 {
			t.Errorf("entry %d: position %d", i, e.Position)
		}
		if e.Song.ID == 0 || e.Song.Title == "" || e.Song.Artist == "" {
			t.Errorf("entry %d: incomplete song %+v", i, e.Song)
		}
		if e.Trend == "" {
			t.Errorf("entry %d: no trend", i)
		}
	}
	var found bool
	for _, e := range entries {
		if e.Song.ID == 126112 {
			found = true
			if e.Position != 27 || e.Song.Title != "Nassanga (Remix)" || e.Trend != "up" ||
				*e.LastDay != 27 || *e.LastWeek != 31 || *e.Peak != 1 || *e.WeeksOnChart != 17 {
				t.Errorf("unexpected entry: %+v", e)
			}
		}
	}
	if !found {
		t.Error("song 126112 not found")
	}
}

func TestParseListing(t *testing.T) {
	lp, err := ParseListing(fixture(t, "audio_new.html"), "/audio/new", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lp.Songs) != 36 {
		t.Fatalf("got %d songs, want 36", len(lp.Songs))
	}
	if !lp.HasNext {
		t.Error("expected HasNext")
	}
	s := lp.Songs[0]
	if s.ID != 130077 || s.Title != "Kulu Kuluba" || s.Artist != "J Kax" || s.ArtistID != 26903 {
		t.Errorf("unexpected first song: %+v", s)
	}
}

func TestParseSong(t *testing.T) {
	d, err := ParseSong(fixture(t, "song.html"),
		"https://www.howwe.ug/song/126112/nassanga-remix/93/kalifah-aganaga-ft.-eddy-kenzo-winnie-nwagi-lil-pazo-weasel")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://www.howwe.ug/_fxs_/_upls_/_sngs_/kalifah_aganaga_ft._eddy_kenzo_winnie_nwagi_lil_pazo_weasel-nassanga_remix.mp3?_=1"
	if d.AudioURL != want {
		t.Errorf("audio url = %q, want %q", d.AudioURL, want)
	}
	if d.Title != "Nassanga (Remix)" || d.AddedDate != "July 14, 2026" || d.Album != "Kalifah AgaNaga - Singles" {
		t.Errorf("unexpected detail: %+v", d)
	}
	if d.FileSize != "3.7 MB" || d.Rating == nil || *d.Rating != 4.3 || d.Votes == nil || *d.Votes != 753 {
		t.Errorf("unexpected size/rating: %+v", d)
	}
	if d.VideoURL != "https://www.howwe.ug/watch/video/1866/Oo4FWZItRG4" {
		t.Errorf("video url = %q", d.VideoURL)
	}
}

// A song reached by id alone (ProbePath) still yields its canonical path.
func TestParseSongProbe(t *testing.T) {
	d, err := ParseSong(fixture(t, "song_probe.html"), "https://www.howwe.ug"+ProbePath(300))
	if err != nil {
		t.Fatal(err)
	}
	c := d.Canonical
	if d.Title != "Time Bomb" || c.ID != 300 || c.Slug != "time-bomb" || c.ArtistID != 77 || c.ArtistSlug != "bobi-wine" {
		t.Fatalf("unexpected: title=%q canonical=%+v", d.Title, c)
	}
	if d.AudioURL == "" {
		t.Fatal("no audio url")
	}
}

func TestParseSongMissing(t *testing.T) {
	// howwe.ug answers 200 with an empty song shell for unknown ids.
	body := []byte(`<html><head><base href='https://www.howwe.ug'></head><body><h1></h1></body></html>`)
	if _, err := ParseSong(body, "https://www.howwe.ug"+ProbePath(999999)); err == nil {
		t.Fatal("want error for empty song page")
	}
}

func TestParseGenres(t *testing.T) {
	gs, err := ParseGenres(fixture(t, "genres.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 42 {
		t.Fatalf("got %d genres, want 42", len(gs))
	}
	total, byName := 0, map[string]Genre{}
	for _, g := range gs {
		total += g.SongCount
		byName[g.Name] = g
	}
	if total != 125135 {
		t.Errorf("total songs = %d, want 125135", total)
	}
	if g := byName["Acapella"]; g.Path != "/genres/Acapella" || g.SongCount != 308 || g.Title != "Acapella" {
		t.Errorf("Acapella: %+v", g)
	}
}

func TestParseGenrePage(t *testing.T) {
	lp, err := ParseListing(fixture(t, "genre_acapella.html"), "/genres/Acapella", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lp.Songs) != 36 || !lp.HasNext {
		t.Fatalf("got %d songs, hasNext=%v", len(lp.Songs), lp.HasNext)
	}
	for _, s := range lp.Songs {
		if s.ID == 0 || s.Title == "" {
			t.Fatalf("incomplete song %+v", s)
		}
	}
}
