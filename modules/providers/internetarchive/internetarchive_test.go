package internetarchive

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
)

var site = Site{Base: "https://archive.org", Collections: DefaultCollections, Items: 3}

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

// Live Music Archive: titles live on the original flac; mp3 derivatives
// inherit them, and live recordings are marked as such.
func TestParseConcert(t *testing.T) {
	tracks, err := site.ParseItem("sigurros2005-08-04.aud", fixture(t, "meta_sigurros2005-08-04.aud.json"), true)
	if err != nil || len(tracks) == 0 {
		t.Fatalf("tracks=%d err=%v", len(tracks), err)
	}
	tr := tracks[0]
	if tr.Title != "Intro (Live)" || tr.Artist != "Sigur Rós" || tr.DurationMs != 170000 || tr.ReleasedAt == nil {
		t.Fatalf("%+v", tr)
	}
	if tr.AudioURL != "https://archive.org/download/sigurros2005-08-04.aud/sr2005-08-04d1t01.mp3" ||
		tr.PageURL != "https://archive.org/details/sigurros2005-08-04.aud" || tr.ExternalID != "sigurros2005-08-04.aud/sr2005-08-04d1t01.mp3" {
		t.Fatalf("%+v", tr)
	}
}

// A netlabel release with untitled wav originals: titles come from file names.
func TestParseNetlabelRelease(t *testing.T) {
	tracks, err := site.ParseItem("UN0322", fixture(t, "meta_UN0322.json"), false)
	if err != nil || len(tracks) == 0 {
		t.Fatalf("tracks=%d err=%v", len(tracks), err)
	}
	for _, tr := range tracks {
		if tr.Artist != "serocell" || strings.HasSuffix(tr.Title, ".mp3") || strings.HasPrefix(tr.Title, "0") || tr.DurationMs == 0 || tr.Genre == "" {
			t.Errorf("%+v", tr)
		}
	}
}

func TestFileTitleAndLength(t *testing.T) {
	for in, want := range map[string]string{"04_serocell=harp-zfPKkd.mp3": "serocell=harp-zfPKkd", "01 - Song Name.mp3": "Song Name", "Song.mp3": "Song"} {
		if got := fileTitle(in); got != want {
			t.Errorf("fileTitle(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"184.32": 184320, "02:50": 170000, "1:02:03": 3723000, "": 0} {
		if got := lengthMs(in); got != want {
			t.Errorf("lengthMs(%q) = %d, want %d", in, got, want)
		}
	}
}

// The feed walks items oldest added first, and re-anchors its watermark
// before advancedsearch's 10k paging cap.
func TestFeedCursor(t *testing.T) {
	var queries []url.Values
	get := func(_ context.Context, p string) ([]byte, error) {
		if strings.HasPrefix(p, "/advancedsearch.php?") {
			v, _ := url.ParseQuery(strings.TrimPrefix(p, "/advancedsearch.php?"))
			queries = append(queries, v)
			return fixture(t, "search_page.json"), nil // 3 docs
		}
		return fixture(t, "meta_UN0322.json"), nil
	}
	tracks, next, done, err := site.FeedPage(context.Background(), get, "netlabels", "")
	if err != nil || done || next != "|2" || len(tracks) == 0 {
		t.Fatalf("next=%q done=%v n=%d err=%v", next, done, len(tracks), err)
	}
	if q := queries[0]; q.Get("q") != "collection:(netlabels)" || q["sort[]"][0] != "addeddate asc" || q.Get("rows") != "3" {
		t.Fatalf("query %v", q)
	}
	_, next, _, _ = site.FeedPage(context.Background(), get, "netlabels", "2026-11-01T00:00:00Z|3333")
	if !strings.HasSuffix(next, "|1") || strings.HasPrefix(next, "2026-11-01T00:00:00Z") {
		t.Fatalf("want a re-anchored watermark, got %q", next)
	}
	if !strings.Contains(queries[1].Get("q"), "addeddate:[2026-11-01T00:00:00Z TO null]") || queries[1].Get("page") != "3333" {
		t.Fatalf("query %v", queries[1])
	}
	short := func(_ context.Context, p string) ([]byte, error) {
		if strings.HasPrefix(p, "/advancedsearch.php?") {
			return []byte(`{"response":{"docs":[]}}`), nil
		}
		return nil, nil
	}
	if _, next, done, _ := site.FeedPage(context.Background(), short, "netlabels", "W|4"); !done || next != "W|4" {
		t.Fatalf("end of a tail feed keeps its cursor: next=%q done=%v", next, done)
	}
}
