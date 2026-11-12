package fma

import (
	"context"
	"os"
	"testing"
)

var site = Site{Base: "https://freemusicarchive.org"}

func TestGenreFeedPage(t *testing.T) {
	body, err := os.ReadFile("testdata/genre_hip_hop.html")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	get := func(_ context.Context, p string) ([]byte, error) { path = p; return body, nil }
	tracks, next, done, err := site.FeedPage(context.Background(), get, "Hip-Hop", "")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/genre/Hip-Hop/?page=1" || next != "2" || done {
		t.Fatalf("path=%q next=%q done=%v", path, next, done)
	}
	if len(tracks) != 20 {
		t.Fatalf("got %d tracks, want 20", len(tracks))
	}
	first := tracks[0]
	if first.ExternalID != "287281" || first.Title != "AdBlue" || first.Artist != "Isaï" || first.Genre != "Hip-Hop" ||
		first.AudioURL != "https://freemusicarchive.org/track/adblue/stream/" {
		t.Fatalf("%+v", first)
	}
	// the last page ends the feed
	if _, _, done, _ := site.FeedPage(context.Background(), get, "Hip-Hop", "710"); !done {
		t.Error("page 710 of 710 should end the feed")
	}
}

func TestSearchAndGenreNames(t *testing.T) {
	body, err := os.ReadFile("testdata/search_soul.html")
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := site.ParseSearch(body)
	if err != nil || len(tracks) == 0 {
		t.Fatalf("tracks=%d err=%v", len(tracks), err)
	}
	if site.SearchPath("isaac hayes") != "/search/?quicksearch=isaac+hayes" {
		t.Error("SearchPath")
	}
	if genreName("Soul-RB") != "Soul/R&B" || genreName("Jazz") != "Jazz" {
		t.Error("genreName")
	}
}
