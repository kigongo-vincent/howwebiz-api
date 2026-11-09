package pearltunes

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

var site = Site{Base: "https://pearltunes.com"}

// Compile-time checks: pearltunes supports every optional capability.
var (
	_ sitecrawl.Site       = site
	_ sitecrawl.Prober     = site
	_ sitecrawl.Searchable = site
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseList(t *testing.T) {
	tracks, err := site.ParseList(fixture(t, "list_new_1.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 45 {
		t.Fatalf("got %d tracks, want 45", len(tracks))
	}
	g := tracks[0]
	if g.ExternalID != "15462" || g.Title != "Gundi (Remix)" || g.Artist != "Kapeke" ||
		len(g.Featured) != 1 || g.Featured[0] != "Mimi Kampala" ||
		g.PageURL != "https://pearltunes.com/Kapeke/song/mp3/15462/gundi-remix/kapeke" || !strings.HasSuffix(g.CoverURL, ".webp") {
		t.Fatalf("first: %+v", g)
	}
	for _, tr := range tracks {
		if tr.ExternalID == "" || tr.Title == "" || tr.Artist == "" {
			t.Errorf("incomplete: %+v", tr)
		}
	}
	if past, _ := site.ParseList(fixture(t, "list_past_end.html")); len(past) != 0 {
		t.Fatalf("past the end: %d tracks", len(past))
	}
}

func TestParseSong(t *testing.T) {
	tracks, err := site.ParseSong(fixture(t, "song_15462.html"), 15462)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 16 {
		t.Fatalf("got %d tracks, want 16 (song + artist's others)", len(tracks))
	}
	s := tracks[0]
	if s.ExternalID != "15462" || s.Title != "Gundi (Remix)" || s.Artist != "Kapeke" || s.DurationMs != 179000 ||
		s.AudioURL != "https://pearltunes.com/uploads/songs/kapeke-gundi-remix.mp3" ||
		s.CoverURL != "https://pearltunes.com/uploads/covers/pearltunes-69ef91285092d545326374.webp" {
		t.Fatalf("song: %+v", s)
	}
	for _, tr := range tracks {
		if !strings.HasSuffix(tr.AudioURL, ".mp3") {
			t.Errorf("no mp3: %+v", tr)
		}
	}
	if _, err := site.ParseSong(fixture(t, "song_missing.html"), 99999); !errors.Is(err, sitecrawl.ErrNotFound) {
		t.Fatalf("missing id: want ErrNotFound, got %v", err)
	}
}

func TestParseSearch(t *testing.T) {
	tracks, err := site.ParseSearch(fixture(t, "search_kenzo.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) < 40 {
		t.Fatalf("got %d results", len(tracks))
	}
	kenzo := 0
	for _, tr := range tracks {
		if strings.Contains(strings.ToLower(tr.Artist+" "+tr.Title+" "+strings.Join(tr.Featured, " ")), "kenzo") {
			kenzo++
		}
	}
	if kenzo < len(tracks)/2 {
		t.Fatalf("only %d of %d results mention kenzo", kenzo, len(tracks))
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]int{"2:59": 179000, "1:02:03": 3723000, "": 0, "x": 0} {
		if got := durationMs(in); got != want {
			t.Errorf("durationMs(%q) = %d, want %d", in, got, want)
		}
	}
}
