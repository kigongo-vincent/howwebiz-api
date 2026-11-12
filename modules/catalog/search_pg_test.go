package catalog

import (
	"context"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Typo-tolerant search needs Postgres + pg_trgm. Runs when
// TEST_POSTGRES_DSN points at a throwaway database, e.g.
//
//	docker run -d --rm -e POSTGRES_PASSWORD=pw -p 55432:5432 postgres:16-alpine
//	TEST_POSTGRES_DSN='postgres://postgres:pw@127.0.0.1:55432/postgres?sslmode=disable' go test ./modules/catalog -run Postgres
func TestSearchFuzzyPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	const schema = "catalog_fuzzy_test"
	db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	db.Exec("CREATE SCHEMA " + schema)
	db.Exec("CREATE EXTENSION IF NOT EXISTS pg_trgm")
	t.Cleanup(func() { db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE") })
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // search_path is per connection
	if err := db.Exec("SET search_path TO " + schema + ", public").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Song{}, &Source{}); err != nil {
		t.Fatal(err)
	}
	in := &Ingester{DB: db, Priority: []string{"a"}}
	tracks := []Track{
		{Provider: "a", ExternalID: "1", Title: "Dole", Artist: "Fik Fameica", AudioURL: "1"},
		{Provider: "a", ExternalID: "2", Title: "Dole Y'omwana", Artist: "Fred Ssebata", AudioURL: "2"},
		{Provider: "a", ExternalID: "3", Title: "Nkwagala", Artist: "Eddy Kenzo", AudioURL: "3"},
		{Provider: "a", ExternalID: "4", Title: "Sitya Loss", Artist: "Eddy Kenzo", AudioURL: "4"},
		{Provider: "a", ExternalID: "5", Title: "Chubby Lobby", Artist: "Someone", AudioURL: "5"},
	}
	if _, err := in.Ingest(tracks, t0); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE INDEX ON catalog_songs USING gin (search_text gin_trgm_ops)`).Error; err != nil {
		t.Fatal(err)
	}
	svc := &SearchService{DB: db, Ingester: in, Fuzzy: true}
	if err := svc.PrepareSearchText(context.Background()); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ q, top, match string }{
		{"dole yomwana", "Dole Y'omwana", "exact"},
		{"Dole Y'omwana", "Dole Y'omwana", "exact"},
		{"ssebatta", "Dole Y'omwana", "exact"}, // doubled letters collapse: "sebata"
		{"dole ssebatta", "Dole Y'omwana", "exact"},
		{"fameika", "Dole", "close"},            // misspelled artist (Fameica): typo match
		{"nkwagla", "Nkwagala", "close"},        // missing letter
		{"sitya los", "Sitya Loss", "exact"},    // "los" is in "loss"
		{"kenzo sityaa", "Sitya Loss", "exact"}, // doubled letter collapses
		{"nkwagla bby", "Nkwagala", "close"},    // a 3-letter fragment inside "chubby" ranks below the typo match
	}
	for _, c := range cases {
		res, err := svc.Search(context.Background(), Query{Q: c.q, Limit: 10, Live: LiveOff})
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		if len(res.Songs) == 0 || res.Songs[0].Title != c.top || res.Match != c.match {
			t.Errorf("%q: match=%q got %v, want %q first (%s)", c.q, res.Match, titles(res), c.top, c.match)
		}
	}
	// no dead ends: nothing as typed and nothing within the usual typo
	// distance ("sebta", similarity 0.5) -> the closest songs, as a close match
	if res, _ := svc.Search(context.Background(), Query{Q: "sebta", Limit: 10, Live: LiveOff}); len(res.Songs) == 0 ||
		res.Songs[0].Title != "Dole Y'omwana" || res.Match != "close" {
		t.Errorf("sebta: match=%q %v", res.Match, titles(res))
	}
	// a word nobody spelled like that, and nothing like it: nothing
	if res, _ := svc.Search(context.Background(), Query{Q: "zzqxv", Limit: 10, Live: LiveOff}); len(res.Songs) != 0 {
		t.Errorf("zzqxv: got %v", titles(res))
	}
}

// After a deploy Redis is empty: the search index must still be found
// (from Postgres) before the first request, or every search scans.
func TestRestoreIndexStatePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	const schema = "catalog_restore_test"
	db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	db.Exec("CREATE SCHEMA " + schema)
	db.Exec("CREATE EXTENSION IF NOT EXISTS pg_trgm")
	t.Cleanup(func() { db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE") })
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("SET search_path TO " + schema + ", public").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Song{}, &Source{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP INDEX IF EXISTS public.idx_catalog_songs_search_trgm") // only this schema's counts
	before := &SearchService{DB: db}
	before.RestoreIndexState(context.Background())
	if before.textIndexed.Load() {
		t.Fatal("no index yet: must not claim the index is ready")
	}
	if err := db.Exec(`CREATE INDEX idx_catalog_songs_search_trgm ON catalog_songs USING gin (search_text gin_trgm_ops)`).Error; err != nil {
		t.Fatal(err)
	}
	after := &SearchService{DB: db} // no Redis: as after a deploy
	after.RestoreIndexState(context.Background())
	if !after.textIndexed.Load() {
		t.Fatal("the index exists: search must use it from the first request")
	}
}
