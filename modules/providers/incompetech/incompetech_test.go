package incompetech

import (
	"context"
	"os"
	"testing"
)

func TestFeed(t *testing.T) {
	body, err := os.ReadFile("testdata/pieces.json")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	get := func(_ context.Context, p string) ([]byte, error) { path = p; return body, nil }
	tracks, _, done, err := Site{Base: "https://incompetech.com"}.FeedPage(context.Background(), get, "pieces", "")
	if err != nil || !done || path != "/music/royalty-free/pieces.json" {
		t.Fatalf("err=%v done=%v path=%q", err, done, path)
	}
	if len(tracks) != 5 { // the sixth has no file
		t.Fatalf("got %d tracks, want 5", len(tracks))
	}
	tr := tracks[0]
	if tr.Title != "The Britons" || tr.Artist != Artist || tr.DurationMs != 307000 || tr.ExternalID != "USUAN2600004" ||
		tr.AudioURL != "https://incompetech.com/music/royalty-free/mp3-royaltyfree/The%20Britons.mp3" {
		t.Fatalf("%+v", tr)
	}
}
