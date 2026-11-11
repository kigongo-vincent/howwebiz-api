package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/cache"
)

func searchFixture(t *testing.T) (*SearchService, *Ingester) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"howwe", "momuziki"}}
	if _, err := in.Ingest([]Track{
		{Provider: "howwe", ExternalID: "1", Title: "Sitya Loss", Artist: "Éddy Kénzo", AudioURL: "a1"},
		{Provider: "howwe", ExternalID: "2", Title: "Nassanga (Remix)", Artist: "Kalifah AgaNaga ft. Winnie Nwagi", AudioURL: "a2"},
		{Provider: "momuziki", ExternalID: "3", Title: "Bweyagala", Artist: "Vinka", AudioURL: "a3"},
	}, t0); err != nil {
		t.Fatal(err)
	}
	return &SearchService{DB: db, Ingester: in}, in
}

func titles(r Result) []string {
	out := []string{}
	for _, s := range r.Songs {
		out = append(out, s.Title)
	}
	return out
}

// The indexed column matches everything the per-column query did: accents,
// featured artists, the match key, several words.
func TestSearchTextMatchesLikeTheColumns(t *testing.T) {
	svc, _ := searchFixture(t)
	queries := map[string]int{"kenzo": 1, "EDDY kénzo": 1, "nwagi": 1, "nassanga remix": 1, "sitya-loss": 1, "vinka": 1, "a": 3, "nothing here": 0}
	for _, mode := range []string{"columns", "search_text"} {
		if mode == "search_text" {
			if err := svc.PrepareSearchText(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !svc.textIndexed.Load() {
				t.Fatal("search_text not marked ready")
			}
		}
		for q, want := range queries {
			res, err := svc.Search(context.Background(), Query{Q: q, Live: LiveOff, Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if res.Total != int64(want) || len(res.Songs) != want {
				t.Errorf("%s: %q -> total %d %v, want %d", mode, q, res.Total, titles(res), want)
			}
		}
	}
}

// Rows stored before the column existed are backfilled.
func TestPrepareSearchTextBackfills(t *testing.T) {
	svc, _ := searchFixture(t)
	if err := svc.DB.Exec("UPDATE catalog_songs SET search_text = ''").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.PrepareSearchText(context.Background()); err != nil {
		t.Fatal(err)
	}
	var empty int64
	svc.DB.Model(&Song{}).Where("search_text = ''").Count(&empty)
	if empty != 0 {
		t.Fatalf("%d rows left without search_text", empty)
	}
	var s Song
	svc.DB.Where("title = ?", "Sitya Loss").First(&s)
	if s.SearchText != "sityalos sitya los edy kenzo edy kenzo sitya los edykenzo edykenzosityalos" || s.SearchVersion != SearchTextVersion {
		t.Fatalf("search_text = %q", s.SearchText)
	}
}

func TestSearchServesFromCache(t *testing.T) {
	svc, in := searchFixture(t)
	mr := miniredis.RunT(t)
	svc.Cache, svc.ResultTTL, svc.CountTTL = cache.New(mr.Addr(), "t:"), time.Minute, time.Minute
	ctx := context.Background()
	q := Query{Q: "kenzo", Live: LiveOff}

	first, err := svc.Search(ctx, q)
	if err != nil || first.Cached || first.Total != 1 {
		t.Fatalf("first: cached=%v total=%d err=%v", first.Cached, first.Total, err)
	}
	// a new match arrives; the cached page is still served...
	if _, err := in.Ingest([]Track{{Provider: "momuziki", ExternalID: "9", Title: "Tweyagale", Artist: "Eddy Kenzo", AudioURL: "a9"}}, t0); err != nil {
		t.Fatal(err)
	}
	again, _ := svc.Search(ctx, Query{Q: " KENZO ", Live: LiveOff}) // same query, other spelling
	if !again.Cached || again.Total != 1 || again.Songs[0].Title != "Sitya Loss" {
		t.Fatalf("second: cached=%v total=%d %v", again.Cached, again.Total, titles(again))
	}
	// ...until a bump (reindex/cleanup) invalidates it
	svc.Bump(ctx)
	fresh, _ := svc.Search(ctx, q)
	if fresh.Cached || fresh.Total != 2 {
		t.Fatalf("after bump: cached=%v total=%d", fresh.Cached, fresh.Total)
	}
	// Redis gone: same answers, uncached
	mr.Close()
	down, err := svc.Search(ctx, q)
	if err != nil || down.Cached || down.Total != 2 {
		t.Fatalf("redis down: cached=%v total=%d err=%v", down.Cached, down.Total, err)
	}
}

// "dole yomwana" must find "Dole Y'omwana" first: the apostrophe splits the
// stored word, the user types it joined.
func TestSearchSquashedSpelling(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"a"}}
	if _, err := in.Ingest([]Track{
		{Provider: "a", ExternalID: "1", Title: "Dole", Artist: "Fik Fameica", AudioURL: "1"},
		{Provider: "a", ExternalID: "2", Title: "Dole Y'omwana", Artist: "Fred Ssebata", AudioURL: "2"},
		{Provider: "a", ExternalID: "3", Title: "Omwana Wange", Artist: "Someone", AudioURL: "3"},
	}, t0); err != nil {
		t.Fatal(err)
	}
	svc := &SearchService{DB: db, Ingester: in}
	if err := svc.PrepareSearchText(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"dole yomwana", "Dole Y'omwana", "dole y omwana", "doleyomwana", "DOLE YOMWANA ssebata"} {
		res, err := svc.Search(context.Background(), Query{Q: q, Limit: 10, Live: LiveOff})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Songs) == 0 || res.Songs[0].Title != "Dole Y'omwana" || res.Match != "exact" {
			t.Errorf("%q: match=%q got %v", q, res.Match, titles(res))
		}
	}
}

// Mixes stay out of song searches unless asked for.
func TestSearchTypeFilter(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"a"}}
	if _, err := in.Ingest([]Track{
		{Provider: "a", ExternalID: "1", Title: "Kadongo Kamu Nonstop", Artist: "DJ Zero", AudioURL: "1"},
		{Provider: "a", ExternalID: "2", Title: "Walumbe Zaaya", Artist: "Paulo Kafeero", AudioURL: "2", Genre: "kadongo kamu"},
		{Provider: "a", ExternalID: "3", Title: "Kadongo Kamu Classics", Artist: "Various", AudioURL: "3", Kind: KindMix}, // the provider says so
	}, t0); err != nil {
		t.Fatal(err)
	}
	svc := &SearchService{DB: db, Ingester: in}
	count := func(typ string) int {
		res, err := svc.Search(context.Background(), Query{Q: "kadongo", Type: typ, Limit: 10, Live: LiveOff})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Songs)
	}
	if got := count(""); got != 0 {
		t.Errorf("default (songs): %d results, want 0 (both kadongo results are mixes)", got)
	}
	if got := count(KindMix); got != 2 {
		t.Errorf("mixes: %d, want 2", got)
	}
	if got := count("all"); got != 2 {
		t.Errorf("all: %d, want 2", got)
	}
	res, _ := svc.Search(context.Background(), Query{Q: "walumbe", Limit: 10, Live: LiveOff})
	if len(res.Songs) != 1 || res.Songs[0].Kind != KindSong {
		t.Errorf("walumbe: %v", titles(res))
	}
}

// Luganda spellings vary by doubled letters; the song whose title IS the
// query beats songs that merely contain it (titles as Mdundo has them).
func TestSearchDoubledLettersAndExactTitle(t *testing.T) {
	db := testDB(t)
	in := &Ingester{DB: db, Priority: []string{"mdundo"}}
	if _, err := in.Ingest([]Track{
		{Provider: "mdundo", ExternalID: "1", Title: "Gudi Gude & Dole Yomwana - Live", Artist: "Kalifah Aganaga", AudioURL: "1"},
		{Provider: "mdundo", ExternalID: "2", Title: "Dole Y'omwaana", Artist: "Lord Fred Sebatta", AudioURL: "2"},
		{Provider: "mdundo", ExternalID: "3", Title: "Dole Dale (All Night)", Artist: "Freeboy", AudioURL: "3"},
		{Provider: "mdundo", ExternalID: "4", Title: "Rukundo yomwana", Artist: "Mr Jay", AudioURL: "4"},
	}, t0); err != nil {
		t.Fatal(err)
	}
	svc := &SearchService{DB: db, Ingester: in}
	if err := svc.PrepareSearchText(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"dole yomwana", "dole yomwaana", "Dole Y'omwana", "dole yomwana ssebata", "dole yomwana sebatta"} {
		res, err := svc.Search(context.Background(), Query{Q: q, Limit: 10, Live: LiveOff})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Songs) == 0 || res.Songs[0].Title != "Dole Y'omwaana" || res.Match != "exact" {
			t.Errorf("%q: match=%q %v", q, res.Match, titles(res))
		}
	}
	if got := SearchFold("Ssebatta Omwaana 2000"); got != "sebata omwana 2000" {
		t.Errorf("SearchFold: %q", got)
	}
}
