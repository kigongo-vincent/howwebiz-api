package hearthis

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

func TestCategoryFeed(t *testing.T) {
	body, err := os.ReadFile("testdata/category_hiphop.json")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	get := func(_ context.Context, p string) ([]byte, error) { path = p; return body, nil }
	tracks, next, done, err := Site{Base: "https://api-v2.hearthis.at"}.FeedPage(context.Background(), get, "hiphop", "")
	if err != nil || done || next != "2" || path != "/categories/hiphop/?page=1&count=20" {
		t.Fatalf("err=%v done=%v next=%q path=%q", err, done, next, path)
	}
	if len(tracks) == 0 {
		t.Fatal("no tracks")
	}
	first := tracks[0]
	if first.ExternalID != "14756636" || first.Genre != "Hip Hop" || first.DurationMs != 219000 || first.Artist == "" ||
		!strings.HasPrefix(first.AudioURL, "https://hearthis.app/") || first.CoverURL == "" {
		t.Fatalf("%+v", first)
	}
	// hour-long DJ sets are mixes by length (the catalog decides)
	var mixes int
	for _, tr := range tracks {
		if catalog.KindOf(tr) == catalog.KindMix {
			mixes++
		}
	}
	if mixes == 0 || catalog.KindOf(first) != catalog.KindSong {
		t.Errorf("mixes=%d first=%s", mixes, catalog.KindOf(first))
	}
	empty := func(_ context.Context, _ string) ([]byte, error) { return []byte("[]"), nil }
	if _, _, done, _ := (Site{}).FeedPage(context.Background(), empty, "hiphop", "99"); !done {
		t.Error("an empty page ends the feed")
	}
}

func TestSearch(t *testing.T) {
	body, _ := os.ReadFile("testdata/search_soul.json")
	var path string
	get := func(_ context.Context, p string) ([]byte, error) { path = p; return body, nil }
	tracks, err := Site{}.SearchAPI(context.Background(), get, "soul", 5)
	if err != nil || len(tracks) != 5 || path != "/search/?count=5&page=1&t=soul" {
		t.Fatalf("tracks=%d path=%q err=%v", len(tracks), path, err)
	}
}

// Some endpoints send numbers where others send strings.
func TestNumbersOrStrings(t *testing.T) {
	body := []byte(`[{"id":123,"title":"Smooth","duration":245,"playback_count":1000,"genre":"RnB","stream_url":"https://hearthis.app/x/smooth/listen/","user":{"username":"DJ X"}},
	{"id":"124","title":"Silk","duration":"200","playback_count":null,"stream_url":"https://hearthis.app/x/silk/listen/"}]`)
	items, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tracks(items)
	if len(tr) != 2 || tr[0].ExternalID != "123" || tr[0].DurationMs != 245000 || tr[0].Plays == nil || *tr[0].Plays != 1000 ||
		tr[1].ExternalID != "124" || tr[1].Plays != nil {
		t.Fatalf("%+v", tr)
	}
}
