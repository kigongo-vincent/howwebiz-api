package jamendo

import (
	"context"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

var (
	_ sitecrawl.Site        = Site{}
	_ sitecrawl.Feeder      = Site{}
	_ sitecrawl.APISearcher = Site{}
	_ sitecrawl.Enabler     = Site{}
)

// Shape from https://developer.jamendo.com/v3.0/tracks (sample response).
const page = `{"headers":{"status":"success","code":0,"error_message":"","warnings":"","results_count":2},
"results":[{"id":"1886257","name":"Sunrise","duration":214,"artist_id":"7","artist_name":"Ayo","album_name":"",
"album_image":"","image":"https://usercontent.jamendo.com?type=album&id=1&width=600","audio":"https://prod-1.storage.jamendo.com/?trackid=1886257&format=mp32",
"audiodownload":"","audiodownload_allowed":false,"releasedate":"2021-04-11","shareurl":"https://www.jamendo.com/track/1886257",
"license_ccurl":"http://creativecommons.org/licenses/by-nc-nd/3.0/","musicinfo":{"tags":{"genres":["afrobeat","pop"]}}},
{"id":"2","name":"No Audio","duration":10,"artist_name":"X","audio":""}]}`

func TestParse(t *testing.T) {
	n, tracks, err := Parse([]byte(page))
	if err != nil || n != 2 || len(tracks) != 1 {
		t.Fatalf("n=%d tracks=%d err=%v", n, len(tracks), err)
	}
	tr := tracks[0]
	if tr.ExternalID != "1886257" || tr.Artist != "Ayo" || tr.Genre != "afrobeat" || tr.DurationMs != 214000 ||
		tr.ReleasedAt == nil || !strings.Contains(tr.AudioURL, "trackid=1886257") || tr.PageURL != "https://www.jamendo.com/track/1886257" {
		t.Fatalf("%+v", tr)
	}
}

func TestAPIErrorIsAnError(t *testing.T) {
	body := `{"headers":{"status":"failed","code":11,"error_message":"Your application has been suspended","results_count":0},"results":[]}`
	if _, _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("want error, got %v", err)
	}
}

func TestTailFeed(t *testing.T) {
	s := Site{Base: "https://api.jamendo.com", ClientID: "abc"}
	var path string
	get := func(_ context.Context, p string) ([]byte, error) { path = p; return []byte(page), nil }
	_, next, done, err := s.FeedPage(context.Background(), get, "all", "400")
	// 2 results < 200: the end; resume at 402 next run.
	if err != nil || !done || next != "402" || !s.Tail("all") {
		t.Fatalf("next=%q done=%v err=%v", next, done, err)
	}
	for _, want := range []string{"client_id=abc", "order=id_asc", "offset=400", "limit=200"} {
		if !strings.Contains(path, want) {
			t.Errorf("path %q lacks %q", path, want)
		}
	}
	if (Site{}).Enabled() || !s.Enabled() {
		t.Error("enabled only with a client id")
	}
}
