package audius

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

var site = Site{Base: "https://api.audius.co", AppName: "test", Genres: Genres}

var (
	_ sitecrawl.Site        = site
	_ sitecrawl.Feeder      = site
	_ sitecrawl.APISearcher = site
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseFeed(t *testing.T) {
	n, tracks, err := site.Parse(fixture(t, "feed_reggae.json"))
	if err != nil || n != 3 || len(tracks) == 0 {
		t.Fatalf("n=%d tracks=%d err=%v", n, len(tracks), err)
	}
	for _, tr := range tracks {
		if tr.Provider != Name || tr.ExternalID == "" || tr.Title == "" || tr.Artist == "" || tr.DurationMs == 0 {
			t.Errorf("incomplete track %+v", tr)
		}
		// A stable full-track URL, not a signed (expiring) node URL.
		if tr.AudioURL != "https://api.audius.co/v1/tracks/"+tr.ExternalID+"/stream?app_name=test" {
			t.Errorf("audio url %q", tr.AudioURL)
		}
		if !strings.HasPrefix(tr.PageURL, "https://audius.co/") || tr.ReleasedAt == nil || tr.Plays == nil {
			t.Errorf("page/release/plays missing: %+v", tr)
		}
	}
}

// Gated tracks only give a preview to anonymous listeners: never stored.
func TestSkipsStreamGated(t *testing.T) {
	n, tracks, err := site.Parse(fixture(t, "feed_gated.json"))
	if err != nil || n != 2 || len(tracks) != 1 || tracks[0].ExternalID != "EvMobj4" {
		t.Fatalf("n=%d tracks=%+v err=%v", n, tracks, err)
	}
}

func TestFeedPaging(t *testing.T) {
	var paths []string
	get := func(_ context.Context, p string) ([]byte, error) {
		paths = append(paths, p)
		return fixture(t, "feed_reggae.json"), nil // a short page is not the end
	}
	tracks, next, done, err := site.FeedPage(context.Background(), get, "Reggae", "200")
	if err != nil || done || next != "300" || len(tracks) == 0 {
		t.Fatalf("next=%q done=%v n=%d err=%v", next, done, len(tracks), err)
	}
	if !strings.Contains(paths[0], "genre=Reggae") || !strings.Contains(paths[0], "offset=200") || !strings.Contains(paths[0], "sort_method=recent") {
		t.Errorf("path %q", paths[0])
	}
	_, _, _, _ = site.FeedPage(context.Background(), get, "all", "")
	if strings.Contains(paths[1], "genre=") {
		t.Errorf("all feed must not filter by genre: %q", paths[1])
	}
	// The search index stops at 10k: the last page ends the feed.
	full := func(_ context.Context, p string) ([]byte, error) {
		return []byte(`{"data":[` + strings.Repeat(`{"id":"x","title":"t"},`, 99) + `{"id":"y","title":"t"}]}`), nil
	}
	if _, _, done, _ := site.FeedPage(context.Background(), full, "all", "9900"); !done {
		t.Error("offset 9900 is the last page under the 10k window: want done")
	}
	if _, _, done, _ := site.FeedPage(context.Background(), full, "all", "0"); done {
		t.Error("a full first page is not the end")
	}
	empty := func(context.Context, string) ([]byte, error) { return []byte(`{"data":[]}`), nil }
	if _, _, done, _ := site.FeedPage(context.Background(), empty, "Kids", "300"); !done {
		t.Error("an empty page ends the feed")
	}
}

func TestVerifiedOnly(t *testing.T) {
	strict := site
	strict.VerifiedOnly = true
	_, all, _ := site.Parse(fixture(t, "feed_reggae.json"))
	_, verified, _ := strict.Parse(fixture(t, "feed_reggae.json"))
	if len(verified) != 0 || len(all) == 0 { // the fixture's uploaders are unverified
		t.Fatalf("verified=%d all=%d", len(verified), len(all))
	}
}

func TestChartFeeds(t *testing.T) {
	s := Site{Base: "https://api.audius.co", AppName: "howwebiz", Genres: []string{"Rock"}, Charts: []string{"R&B/Soul"}}
	feeds := s.Feeds()
	if len(feeds) != 3 || feeds[1] != "trending:R&B/Soul:allTime" || feeds[2] != "trending:R&B/Soul:month" {
		t.Fatalf("feeds %v", feeds)
	}
	body, err := os.ReadFile("testdata/trending_rnb.json")
	if err != nil {
		t.Fatal(err)
	}
	var path string
	get := func(_ context.Context, p string) ([]byte, error) { path = p; return body, nil }
	tracks, next, done, err := s.FeedPage(context.Background(), get, "trending:R&B/Soul:allTime", "")
	if err != nil || !done || next != "" || len(tracks) == 0 {
		t.Fatalf("tracks=%d done=%v next=%q err=%v", len(tracks), done, next, err)
	}
	if path != "/v1/tracks/trending?app_name=howwebiz&genre=R%26B%2FSoul&time=allTime" {
		t.Fatalf("path %q", path)
	}
}
