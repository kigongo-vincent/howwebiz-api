package busoga

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

var site = Site{Base: "https://www.busogamusicstore.com"}

func read(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSong(t *testing.T) {
	tr, err := site.ParseSong(read(t, "song_1004.html"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ExternalID != "1004" || tr.Title != "Namala" || tr.Artist != "B2C" ||
		tr.AudioURL != "https://www.busogamusicstore.com/Africa%20Music/B2c/Namala%20by%20B2C.mp3" ||
		tr.CoverURL != "https://www.busogamusicstore.com/Artist%20Image/B2c/namala-by-b2c.webp" ||
		tr.ReleasedAt == nil || tr.DurationMs != 0 {
		t.Fatalf("%+v", tr)
	}
	if _, err := site.ParseSong(read(t, "song_gone.html")); err == nil {
		t.Error("410 page parsed as a song")
	}
}

func TestFeedReadsListedSongs(t *testing.T) {
	listing := read(t, "music.html")
	paths := SongPaths(listing)
	if len(paths) < 20 || !strings.HasPrefix(paths[0], "/listen-music.php?songid=") || !strings.Contains(paths[0], "&sn=") {
		t.Fatalf("paths: %d %v", len(paths), paths[:min(3, len(paths))])
	}
	song := read(t, "song_1004.html")
	fetched := 0
	get := func(_ context.Context, p string) ([]byte, error) {
		if p == "/music.php" {
			return listing, nil
		}
		fetched++
		if fetched == 2 {
			return nil, fmt.Errorf("GET %s: %w", p, webguard.ErrNotFound) // removed since listed: skipped
		}
		return song, nil
	}
	tracks, _, done, err := site.FeedPage(context.Background(), get, "/music.php", "")
	if err != nil || !done || fetched != len(paths) || len(tracks) != len(paths)-1 {
		t.Fatalf("tracks=%d fetched=%d paths=%d done=%v err=%v", len(tracks), fetched, len(paths), done, err)
	}
}
