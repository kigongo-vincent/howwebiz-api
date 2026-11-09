package momuziki

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

var siteBase, _ = url.Parse("https://www.momuziki.com/")

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseCategories(t *testing.T) {
	cs, err := ParseCategories(fixture(t, "categories.html"), siteBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 18 {
		t.Fatalf("got %d categories, want 18", len(cs))
	}
	if c := cs[0]; c.ID != 8 || c.Name != "Afro Beat" || c.ImageURL != "https://www.momuziki.com/music/images/thumbs/93758_category.jpg" {
		t.Errorf("first category: %+v", c)
	}
}

func TestParseCategoryRows(t *testing.T) {
	rows, err := ParseRows(fixture(t, "category_8.html"), siteBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 30 {
		t.Fatalf("got %d rows, want 30", len(rows))
	}
	r := rows[0]
	if r.ID != 5991 || r.Title != "Yeffe" || r.Artist != "Martha Mukisa" ||
		r.ThumbURL != "https://www.momuziki.com/music/images/thumbs/1794209869_34004_mp3_thumb.jpg" ||
		r.AudioURL != "https://momuziki.com/music/uploads/Martha%20Mukisa%20-%20Yeffe%20%28Official%20Audio%29%28MP3_160K%29.mp3" {
		t.Errorf("first row: %+v", r)
	}
	for _, r := range rows {
		if r.ID == 0 || r.Title == "" || r.Artist == "" || !strings.HasSuffix(strings.ToLower(r.AudioURL), ".mp3") || strings.Contains(r.AudioURL, " ") {
			t.Errorf("incomplete row: %+v", r)
		}
	}
}

func TestParsePastEnd(t *testing.T) {
	rows, err := ParseRows(fixture(t, "category_8_past_end.html"), siteBase)
	if err != nil || len(rows) != 0 {
		t.Fatalf("past the last page: rows=%d err=%v", len(rows), err)
	}
}

func TestParseSearch(t *testing.T) {
	rows, err := ParseRows(fixture(t, "search_kenzo.html"), siteBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 40 {
		t.Fatalf("got %d rows, want 40", len(rows))
	}
	kenzo := 0
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Artist+r.Title), "kenzo") {
			kenzo++
		}
	}
	if kenzo < 30 {
		t.Errorf("only %d of 40 results mention kenzo", kenzo)
	}
}

func TestParseSong(t *testing.T) {
	p, err := ParseSong(fixture(t, "song_5989.html"), siteBase)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Embilizi" || p.Artist != "Echoled" || p.Plays == nil || *p.Plays != 8 ||
		p.ImageURL != "https://www.momuziki.com/music/images/1794209366_29890_mp3_thumb.jpeg" ||
		p.AudioURL != "https://momuziki.com/music/uploads/Echoled%20-%20Embilizi%20%28%20Audio%20%29%28MP3_160K%29.mp3" {
		t.Errorf("song: %+v plays=%v", p, p.Plays)
	}
	if _, err := ParseSong(fixture(t, "song_missing.html"), siteBase); err != ErrNoSong {
		t.Errorf("missing song: want ErrNoSong, got %v", err)
	}
}
