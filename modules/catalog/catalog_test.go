package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&Song{}, &Source{}, &Watermark{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func ip(n int) *int { return &n }

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func TestIngestMergesAcrossProviders(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"howwe", "momuziki", "pearltunes"}}

	st, err := in.Ingest([]Track{
		{Provider: "momuziki", ExternalID: "5991", Title: "Yeffe (Official Audio)", Artist: "Martha Mukisa",
			AudioURL: "https://momuziki/yeffe.mp3", Plays: ip(10), UpdatedAt: t0},
		{Provider: "pearltunes", ExternalID: "777", Title: "Yeffe", Artist: "Martha Mukisa ft. Spice Diana",
			AudioURL: "https://pearl/yeffe.mp3", CoverURL: "https://pearl/c.jpg", Plays: ip(5), DurationMs: 180000, UpdatedAt: t0},
		{Provider: "howwe", ExternalID: "1", Title: "YEFFE", Artist: "martha mukisa", Genre: "AfroBeat", AudioURL: "https://howwe/yeffe.mp3", UpdatedAt: t0},
		{Provider: "howwe", ExternalID: "2", Title: "Other Song", Artist: "Someone", AudioURL: "https://howwe/other.mp3", UpdatedAt: t0},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 4 || st.NewSongs != 2 || st.Attached != 2 {
		t.Fatalf("stats %+v", st)
	}
	var songs []Song
	db.Preload("Sources").Order("id").Find(&songs)
	if len(songs) != 2 {
		t.Fatalf("want 2 catalog entries, got %d", len(songs))
	}
	y := songs[0]
	// howwe has priority for display and audio.
	if y.Title != "YEFFE" || y.SourceCount != 3 || y.Genre != "AfroBeat" || y.AudioURL != "https://howwe/yeffe.mp3" ||
		y.CoverURL != "https://pearl/c.jpg" || y.Plays != 15 || y.DurationMs != 180000 ||
		!slices.Equal(y.Providers, []string{"howwe", "momuziki", "pearltunes"}) || !slices.Equal(y.Featured, []string{"Spice Diana"}) {
		t.Fatalf("aggregate: %+v", y)
	}

	// Re-ingesting identical data writes nothing.
	st, _ = in.Ingest([]Track{{Provider: "howwe", ExternalID: "2", Title: "Other Song", Artist: "Someone", AudioURL: "https://howwe/other.mp3", UpdatedAt: t0}}, t0)
	if st.Unchanged != 1 || st.Updated != 0 {
		t.Fatalf("unchanged: %+v", st)
	}

	// A provider retitles its copy: it moves to the right entry and the
	// emptied entry disappears.
	st, _ = in.Ingest([]Track{{Provider: "howwe", ExternalID: "2", Title: "Yeffe", Artist: "Martha Mukisa", AudioURL: "https://howwe/other.mp3", UpdatedAt: t0.Add(time.Hour)}}, t0)
	var n int64
	db.Model(&Song{}).Count(&n)
	var moved Song
	db.First(&moved, y.ID)
	if st.Updated != 1 || n != 1 || moved.SourceCount != 4 {
		t.Fatalf("re-key: stats=%+v songs=%d sourceCount=%d", st, n, moved.SourceCount)
	}
}

// fakeStore pages through tracks ordered by (UpdatedAt, ExternalID).
type fakeStore struct {
	name   string
	tracks []Track
	calls  atomic.Int64
}

func (f *fakeStore) Name() string { return f.name }
func (f *fakeStore) Changed(_ context.Context, since time.Time, after string, limit int) ([]Track, error) {
	f.calls.Add(1)
	var out []Track
	for _, t := range f.tracks {
		a, _ := strconv.Atoi(after)
		id, _ := strconv.Atoi(t.ExternalID)
		if t.UpdatedAt.After(since) || (t.UpdatedAt.Equal(since) && id > a) {
			out = append(out, t)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func TestIndexerIsIncremental(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"a"}}
	store := &fakeStore{name: "a"}
	for i := 1; i <= 120; i++ {
		store.tracks = append(store.tracks, Track{Provider: "a", ExternalID: strconv.Itoa(i), Title: fmt.Sprintf("Song %d", i), Artist: "X", AudioURL: "a.mp3", UpdatedAt: t0})
	}
	ix := &Indexer{DB: db, Ingester: in, Stores: []Store{store}, Batch: 50}
	res := ix.RunOnce(context.Background())
	if res[0].Stats.Inserted != 120 || res[0].Error != "" || store.calls.Load() != 3 {
		t.Fatalf("first pass: %+v calls=%d", res, store.calls.Load())
	}
	// Nothing changed: one empty page, no ingest.
	res = ix.RunOnce(context.Background())
	if res[0].Stats != (IngestStats{}) || store.calls.Load() != 4 {
		t.Fatalf("second pass: %+v", res)
	}
	// One track changes later: only it is re-read.
	store.tracks = append(store.tracks, Track{Provider: "a", ExternalID: "7", Title: "Song 7 (Remix)", Artist: "X", AudioURL: "a.mp3", UpdatedAt: t0.Add(time.Minute)})
	res = ix.RunOnce(context.Background())
	if res[0].Stats.Updated != 1 || res[0].Stats.NewSongs != 1 {
		t.Fatalf("third pass: %+v", res)
	}
}

type fakeSearcher struct {
	name  string
	delay time.Duration
	err   error
	hits  []Track
	calls atomic.Int64
}

func (f *fakeSearcher) Name() string { return f.name }
func (f *fakeSearcher) Search(ctx context.Context, q string, _ int) ([]Track, error) {
	f.calls.Add(1)
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.hits, f.err
}

func TestSearchFansOutAndMerges(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"p1", "p2"}}
	p1 := &fakeSearcher{name: "p1", hits: []Track{{Provider: "p1", ExternalID: "1", Title: "Sitya Loss", Artist: "Eddy Kenzo", AudioURL: "u1"}}}
	p2 := &fakeSearcher{name: "p2", hits: []Track{
		{Provider: "p2", ExternalID: "9", Title: "Sitya Loss (Official Video)", Artist: "Eddy Kenzo", AudioURL: "u9"},
		{Provider: "p2", ExternalID: "10", Title: "Jubilation", Artist: "Eddy Kenzo", AudioURL: "u10"},
		{Provider: "p2", ExternalID: "11", Title: "Sitya Loss", Artist: "Eddy Kenzo"}}} // no audio: dropped
	slow := &fakeSearcher{name: "slow", delay: time.Second}
	off := &fakeSearcher{name: "off", err: ErrUnavailable}
	broken := &fakeSearcher{name: "broken", err: errors.New("boom")}
	svc := &SearchService{DB: db, Ingester: in, Searchers: []Searcher{p1, p2, slow, off, broken}, Timeout: 50 * time.Millisecond, CacheTTL: time.Hour}

	res, err := svc.Search(context.Background(), Query{Q: "kenzo sitya", Page: 1, Limit: 20, Live: LiveAuto})
	if err != nil {
		t.Fatal(err)
	}
	// the merged song leads; the artist's other song follows (results blend)
	if res.Match != "exact" || res.Songs[0].Title != "Sitya Loss" || res.Songs[0].SourceCount != 2 || len(res.Songs[0].Sources) != 2 {
		t.Fatalf("want the merged entry first, got match=%q total=%d %+v", res.Match, res.Total, res.Songs)
	}
	if res.Total != 2 || res.Songs[1].Title != "Jubilation" {
		t.Fatalf("want the artist's other song second, got total=%d", res.Total)
	}
	status := map[string]string{}
	for _, l := range res.Live {
		status[l.Provider] = l.Status
	}
	if status["p1"] != "ok" || status["p2"] != "ok" || status["slow"] != "timeout" || status["off"] != "skipped" || status["broken"] != "error" {
		t.Fatalf("live statuses: %v", status)
	}
	if res.Ingested.NewSongs != 2 || res.Ingested.Attached != 1 {
		t.Fatalf("ingested: %+v", res.Ingested)
	}

	// Same query again: providers are not asked twice within the cache TTL.
	res, _ = svc.Search(context.Background(), Query{Q: "KENZO  Sitya", Page: 1, Limit: 20, Live: LiveAuto})
	if p1.calls.Load() != 1 || res.Live[0].Status != "cached" {
		t.Fatalf("cache: calls=%d live=%+v", p1.calls.Load(), res.Live)
	}
	// Page 2 and live=false never fan out; artist filter and accents.
	res, _ = svc.Search(context.Background(), Query{Q: "jubilation", Page: 2, Limit: 20, Live: LiveAuto})
	if res.Live != nil {
		t.Fatal("page 2 should not search live")
	}
	res, _ = svc.Search(context.Background(), Query{Artist: "Eddy Kénzo", Sort: "title", Page: 1, Limit: 20, Live: LiveOff})
	if res.Total != 2 || res.Songs[0].Title != "Jubilation" {
		t.Fatalf("artist filter: %+v", res.Songs)
	}
	res, _ = svc.Search(context.Background(), Query{Provider: "p2", Page: 1, Limit: 20, Live: LiveOff})
	if res.Total != 2 {
		t.Fatalf("provider filter: %d", res.Total)
	}
}

// A listing page (no mp3) re-ingested after the song page (with mp3)
// must not erase the mp3.
func TestIngestSparseDataNeverErases(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"p"}}
	in.Ingest([]Track{{Provider: "p", ExternalID: "1", Title: "A", Artist: "B", AudioURL: "u.mp3", DurationMs: 1000, Plays: ip(3)}}, t0) // later: listing without audio
	st, _ := in.Ingest([]Track{{Provider: "p", ExternalID: "1", Title: "A", Artist: "B"}}, t0)
	var src Source
	db.First(&src)
	if st.Unchanged != 1 || src.AudioURL != "u.mp3" || src.DurationMs != 1000 || src.Plays == nil {
		t.Fatalf("stats=%+v source=%+v", st, src)
	}
}

// Relevance: exact title > title prefix > primary artist > other matches,
// regardless of how many providers back an entry.
func TestSearchRelevanceOrder(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"a", "b"}}
	in.Ingest([]Track{
		// featured only, but on two providers (would win on source_count)
		{Provider: "a", ExternalID: "1", Title: "Nassanga", Artist: "Kalifah ft. Eddy Kenzo", AudioURL: "1"},
		{Provider: "b", ExternalID: "1", Title: "Nassanga", Artist: "Kalifah", AudioURL: "1"},
		{Provider: "a", ExternalID: "2", Title: "Sitya Loss", Artist: "Eddy Kenzo", AudioURL: "2"},
		{Provider: "a", ExternalID: "3", Title: "Eddy Kenzo", Artist: "Someone", AudioURL: "3"}, // exact title
	}, t0)
	svc := &SearchService{DB: db, Ingester: in}
	res, err := svc.Search(context.Background(), Query{Q: "eddy kenzo", Page: 1, Limit: 10, Live: LiveOff})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Songs {
		got = append(got, s.Title)
	}
	if !slices.Equal(got, []string{"Eddy Kenzo", "Sitya Loss", "Nassanga"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestIngestRejectsTracksWithoutAudio(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"p"}}
	st, _ := in.Ingest([]Track{
		{Provider: "p", ExternalID: "1", Title: "Has Audio", Artist: "A", AudioURL: "a.mp3"},
		{Provider: "p", ExternalID: "2", Title: "No Audio", Artist: "A"},
		{Provider: "p", ExternalID: "3", Title: "Blank", Artist: "A", AudioURL: "   "},
	}, t0)
	var songs, sources int64
	db.Model(&Song{}).Count(&songs)
	db.Model(&Source{}).Count(&sources)
	if st.Inserted != 1 || st.NoAudio != 2 || songs != 1 || sources != 1 {
		t.Fatalf("stats=%+v songs=%d sources=%d", st, songs, sources)
	}
	// A later listing (no audio) for a stored song keeps it.
	st, _ = in.Ingest([]Track{{Provider: "p", ExternalID: "1", Title: "Has Audio", Artist: "A"}}, t0)
	if st.Unchanged != 1 || st.NoAudio != 0 {
		t.Fatalf("listing re-ingest: %+v", st)
	}
}

func TestPurgeWithoutAudio(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"a", "b"}}
	in.Ingest([]Track{
		{Provider: "a", ExternalID: "1", Title: "Mixed", Artist: "X", AudioURL: "a1.mp3"},
		{Provider: "a", ExternalID: "2", Title: "Silent", Artist: "X", AudioURL: "a2.mp3"},
	}, t0)
	// Legacy rows from before the invariant: a source and a song without audio.
	var mixed, silent Song
	db.Where("key = ?", "x|mixed").First(&mixed)
	db.Where("key = ?", "x|silent").First(&silent)
	db.Create(&Source{SongID: mixed.ID, Provider: "b", ExternalID: "9", Title: "Mixed", Artist: "X"})
	db.Model(&Source{}).Where("external_id = ?", "2").Update("audio_url", "")
	db.Model(&Song{}).Where("id = ?", mixed.ID).Updates(map[string]any{"audio_url": "", "source_count": 2})
	legacy := Song{Key: "y|ghost", Title: "Ghost", Artist: "Y", FirstSeenAt: t0, UpdatedAt: t0}
	db.Create(&legacy) // no sources at all

	st, err := in.PurgeWithoutAudio(t0)
	if err != nil {
		t.Fatal(err)
	}
	var songs []Song
	db.Preload("Sources").Find(&songs)
	if st.SourcesRemoved != 2 || st.SongsRemoved != 2 || len(songs) != 1 {
		t.Fatalf("stats=%+v songs=%+v", st, songs)
	}
	if m := songs[0]; m.ID != mixed.ID || m.AudioURL != "a1.mp3" || m.SourceCount != 1 || len(m.Sources) != 1 {
		t.Fatalf("survivor re-aggregated badly: %+v", m)
	}
	// Idempotent.
	if st, _ := in.PurgeWithoutAudio(t0); st.SourcesRemoved != 0 || st.SongsRemoved != 0 {
		t.Fatalf("second purge: %+v", st)
	}
}

// "dole yomwana": no song holds both words, so the ones holding some of
// them come back, the most words first, and the answer says it's loose
func TestSearchLooseWhenNoSongHasEveryWord(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"a"}}
	in.Ingest([]Track{
		{Provider: "a", ExternalID: "1", Title: "Dole", Artist: "Fik Fameica", AudioURL: "1"},
		{Provider: "a", ExternalID: "2", Title: "Dole Dole Remix", Artist: "Someone", AudioURL: "2"},
		{Provider: "a", ExternalID: "3", Title: "Sitya Loss", Artist: "Eddy Kenzo", AudioURL: "3"},
	}, t0)
	svc := &SearchService{DB: db, Ingester: in}
	res, err := svc.Search(context.Background(), Query{Q: "dole yomwana", Page: 1, Limit: 10, Live: LiveOff})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Loose || res.Total != 2 || len(res.Songs) != 2 {
		t.Fatalf("loose=%v total=%d songs=%d", res.Loose, res.Total, len(res.Songs))
	}
	// a query every word of which matches puts that song first and says
	// so; the songs matching some words follow (results always blend)
	res, _ = svc.Search(context.Background(), Query{Q: "dole fik", Page: 1, Limit: 10, Live: LiveOff})
	if res.Loose || res.Match != "exact" || len(res.Songs) != 2 || res.Songs[0].Title != "Dole" {
		t.Fatalf("exact first: loose=%v match=%q songs=%v", res.Loose, res.Match, res.Songs)
	}
	// an explicit order lists only songs holding every word
	res, _ = svc.Search(context.Background(), Query{Q: "dole fik", Sort: "recent", Page: 1, Limit: 10, Live: LiveOff})
	if len(res.Songs) != 1 || res.Songs[0].Title != "Dole" {
		t.Fatalf("sorted: songs=%v", res.Songs)
	}
}

func TestClipLongFields(t *testing.T) {
	long := strings.Repeat("Ã", 2000)
	if got := clip(long, maxFieldRunes); utf8.RuneCountInString(got) != maxFieldRunes {
		t.Fatalf("clip: %d runes", utf8.RuneCountInString(got))
	}
	if clip("Sitya Loss", maxFieldRunes) != "Sitya Loss" {
		t.Fatal("short value changed")
	}
}
