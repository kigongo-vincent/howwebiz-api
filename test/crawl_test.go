package test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/chart"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
)

// One ordered scenario: the cache layers only make sense across runs.
func TestCrawlCaching(t *testing.T) {
	opts := map[string]any{"targets": []string{"hot100", "audio-new"}, "pages": 1}

	// --- run 1: cold cache, everything is new ---
	songHits := Site.songHits.Load()
	r1 := crawl(t, opts).Stats.Data()
	if r1.SongsInserted == 0 || r1.SongsInserted != r1.SongsSeen {
		t.Fatalf("run1: want every seen song inserted, got %+v", r1)
	}
	if r1.SnapshotsCreated != 2 || r1.DetailsFetched != r1.SongsSeen || r1.DetailsChanged != r1.SongsSeen {
		t.Fatalf("run1: unexpected stats %+v", r1)
	}
	if got := Site.songHits.Load() - songHits; got != int64(r1.SongsSeen) {
		t.Fatalf("run1: %d song page hits, want %d", got, r1.SongsSeen)
	}

	// --- run 2: same content → no writes, no song pages fetched ---
	songHits = Site.songHits.Load()
	r2 := crawl(t, opts).Stats.Data()
	if r2.PagesUnchanged != 2 || r2.SongsInserted != 0 || r2.SongsUpdated != 0 || r2.SnapshotsCreated != 0 || r2.SnapshotsReused != 2 {
		t.Fatalf("run2: expected fully cached listing pass, got %+v", r2)
	}
	if r2.DetailsFetched != 0 || r2.DetailsNotDue != r2.SongsSeen {
		t.Fatalf("run2: expected all song pages to be not due, got %+v", r2)
	}
	if got := Site.songHits.Load() - songHits; got != 0 {
		t.Fatalf("run2: %d song page hits, want 0", got)
	}

	// --- run 3: a title changes on the chart → exactly one song rewritten,
	// chart order unchanged so the snapshot is reused ---
	orig := fixture("hot100.html")
	Site.set("hot100", bytes.Replace(orig, []byte(">Nassanga (Remix)</span>"), []byte(">Nassanga (Remix) [Edit]</span>"), 1))
	r3 := crawl(t, opts).Stats.Data()
	Site.set("hot100", orig)
	if r3.SongsUpdated != 1 || r3.SongsInserted != 0 || r3.SnapshotsCreated != 0 {
		t.Fatalf("run3: want 1 song updated and snapshot reused, got %+v", r3)
	}
	_, data := do(t, http.MethodGet, BASE_URL("howwe/songs/126112"), nil)
	if s := parse[song.Song](t, data).Data; s.Title != "Nassanga (Remix) [Edit]" {
		t.Fatalf("run3: title not updated: %q", s.Title)
	}

	// --- run 4: force bypasses the cache; identical song pages are
	// re-fetched but reported unchanged ---
	r4 := crawl(t, map[string]any{"targets": []string{"hot100"}, "pages": 1, "force": true}).Stats.Data()
	if r4.DetailsFetched != 100 || r4.DetailsChanged != 0 || r4.DetailsUnchanged != 100 {
		t.Fatalf("run4: unexpected force stats %+v", r4)
	}

	// Snapshot history only grew when content changed (run 1 + run 3 restore).
	_, data = do(t, http.MethodGet, BASE_URL("lists/audio-new/snapshots"), nil)
	if snaps := parse[[]chart.Snapshot](t, data); snaps.Pagination.Total != 1 {
		t.Fatalf("audio-new: want 1 snapshot, got %d", snaps.Pagination.Total)
	}
}

func TestReadEndpoints(t *testing.T) {
	crawl(t, map[string]any{"targets": []string{"hot100"}, "pages": 1, "fetchDetails": false})

	res, data := do(t, http.MethodGet, BASE_URL("charts/hot100?page=2&limit=10"), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("hot100: %d %s", res.StatusCode, data)
	}
	chartRes := parse[[]chart.Entry](t, data)
	if len(chartRes.Data) != 10 || chartRes.Data[0].Position != 11 || chartRes.Data[0].Song == nil {
		t.Fatalf("hot100 page 2: unexpected %+v", chartRes.Data[:1])
	}
	if p := chartRes.Pagination; p.Total != 100 || p.TotalPages != 10 || !p.HasMore {
		t.Fatalf("hot100 pagination: %+v", p)
	}

	_, data = do(t, http.MethodGet, BASE_URL("howwe/songs?q=aganaga&hasAudio=true"), nil)
	if songs := parse[[]song.Song](t, data); len(songs.Data) == 0 || songs.Data[0].AudioURL == "" {
		t.Fatalf("song search: %s", data)
	}

	// Provider views serve only songs with audio unless asked otherwise
	// (rows without audio are crawl state: their mp3 is fetched later).
	_, data = do(t, http.MethodGet, BASE_URL("howwe/songs?limit=100"), nil)
	for _, s := range parse[[]song.Song](t, data).Data {
		if s.AudioURL == "" {
			t.Fatalf("default /howwe/songs returned a song without audio: %d", s.ID)
		}
	}
	_, data = do(t, http.MethodGet, BASE_URL("howwe/songs?hasAudio=false&limit=100"), nil)
	for _, s := range parse[[]song.Song](t, data).Data {
		if s.AudioURL != "" {
			t.Fatalf("hasAudio=false returned a song with audio: %d", s.ID)
		}
	}

	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	if res, _ := do(t, http.MethodGet, BASE_URL("lists/hot100?date="+tomorrow), nil); res.StatusCode != http.StatusOK {
		t.Fatalf("hot100 by date: %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, BASE_URL("lists/hot100?date=2000-01-01"), nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("hot100 before any crawl: want 404, got %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, BASE_URL("howwe/songs/1"), nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing song: want 404, got %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, BASE_URL("lists/nope"), nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown list: want 404, got %d", res.StatusCode)
	}
}

func TestCrawlValidationAndBusy(t *testing.T) {
	if res, _ := do(t, http.MethodPost, BASE_URL("crawl"), map[string]any{"targets": []string{"nope"}}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad target: want 400, got %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodPost, BASE_URL("crawl"), map[string]any{"pages": 0}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad pages: want 400, got %d", res.StatusCode)
	}

	Site.slowUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
	defer Site.slowUntil.Store(0)
	res, data := do(t, http.MethodPost, BASE_URL("crawl"), map[string]any{"fetchDetails": false})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %d %s", res.StatusCode, data)
	}
	if res, _ := do(t, http.MethodPost, BASE_URL("crawl"), nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("second crawl: want 409, got %d", res.StatusCode)
	}
	// let it finish so later tests are not blocked
	for i := 0; i < 100; i++ {
		_, data := do(t, http.MethodGet, BASE_URL("crawl/status"), nil)
		if parse[map[string]any](t, data).Data["running"] == false {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("crawl did not finish")
}

// audio-new is newest-first: once page 1 is unchanged and the stored
// snapshot already covers the requested depth, later pages are not fetched.
func TestNewestFirstSkipsDeeperPages(t *testing.T) {
	opts := map[string]any{"targets": []string{"audio-new"}, "pages": 2, "fetchDetails": false}

	first := crawl(t, opts).Stats.Data() // snapshot now covers 2 pages
	if first.PagesFetched != 2 || first.SnapshotsCreated != 1 {
		t.Fatalf("first: %+v", first)
	}
	hits := Site.listHits.Load()
	second := crawl(t, opts).Stats.Data()
	if second.PagesFetched != 1 || second.PagesSkipped != 1 || second.SnapshotsReused != 1 {
		t.Fatalf("second: want page 2 skipped, got %+v", second)
	}
	if got := Site.listHits.Load() - hits; got != 1 {
		t.Fatalf("second: %d listing hits, want 1", got)
	}
}
