package catalog

import (
	"context"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Search quality, measured: a catalog shaped like the real one (popular
// originals next to covers, remixes, live versions, features, look-alike
// titles) and queries the way people type them. Each query names the song
// that must come first, as "Title|primary artist" (features are stored
// apart from the primary artist). Runs on Postgres (TEST_POSTGRES_DSN), where typo
// matching lives. A regression here is a search that got worse.

type evalSong struct {
	title, artist string
	plays         int
	sources       int
}

var evalCatalog = []evalSong{
	{"Sitya Loss", "Eddy Kenzo", 500000, 3},
	{"Sitya Loss (Cover)", "Little Kid Singers", 20, 1},
	{"Sitya Loss Remix", "DJ Ace", 1000, 1},
	{"Mbilo Mbilo", "Eddy Kenzo", 200000, 2},
	{"Nkwagala", "Spice Diana", 90000, 2},
	{"Dole Y'omwaana", "Lord Fred Sebatta", 5000, 1},
	{"Gudi Gude & Dole Yomwana - Live", "Kalifah Aganaga", 300, 1},
	{"Love Nyo", "Fik Fameica", 40000, 2},
	{"Love", "Unknown Singer", 10, 1},
	{"Calm Down", "Rema", 1000000, 3},
	{"Calm Down (Remix)", "Rema ft. Selena Gomez", 800000, 2},
	{"Essence", "Wizkid ft. Tems", 900000, 3},
	{"Last Last", "Burna Boy", 700000, 3},
	{"Suzanna", "Sauti Sol", 300000, 2},
	{"Waah", "Diamond Platnumz ft. Koffi Olomide", 400000, 2},
	{"Kwangwaru", "Harmonize ft. Diamond Platnumz", 350000, 2},
	{"Freed Up", "RUSS", 220000, 1},
	{"bobby hill", "tsuruda", 260000, 1},
	{"Way Up", "Lil HBK", 247000, 1},
	{"Way Up (Remix)", "Somebody Else", 50, 1},
	{"Up", "Random Act", 5, 1},
	{"Tuliyambala Engule", "Bobi Wine ft. All Stars", 50000, 2},
	{"Walumbe Zaaya", "Paulo Kafeero", 30000, 2},
	{"Nassanga (Remix)", "Kalifah AgaNaga ft. Eddy Kenzo, Winnie Nwagi", 120000, 2},
	{"Kenzo Freestyle", "Young Kenz", 100, 1},
	{"Essence of Love", "Small Band", 80, 1},
}

var evalQueries = []struct{ q, want string }{
	{"sitya loss", "Sitya Loss|Eddy Kenzo"},
	{"sitya los", "Sitya Loss|Eddy Kenzo"},
	{"kenzo sitya", "Sitya Loss|Eddy Kenzo"},
	{"eddy kenzo", "Sitya Loss|Eddy Kenzo"},
	{"eddie kenzo", "Sitya Loss|Eddy Kenzo"},
	{"sitya loss remix", "Sitya Loss Remix|DJ Ace"},
	{"calm down", "Calm Down|Rema"},
	{"calmdown", "Calm Down|Rema"},
	{"rema calm down", "Calm Down|Rema"},
	{"calm down selena", "Calm Down (Remix)|Rema"},
	{"essence wizkid", "Essence|Wizkid"},
	{"essense", "Essence|Wizkid"},
	{"wizkid", "Essence|Wizkid"},
	{"tems", "Essence|Wizkid"},
	{"last last", "Last Last|Burna Boy"},
	{"burna boy", "Last Last|Burna Boy"},
	{"diamond platnumz", "Waah|Diamond Platnumz"},
	{"kwangwaru", "Kwangwaru|Harmonize"},
	{"harmonize", "Kwangwaru|Harmonize"},
	{"way up", "Way Up|Lil HBK"},
	{"freed up russ", "Freed Up|RUSS"},
	{"dole yomwana", "Dole Y'omwaana|Lord Fred Sebatta"},
	{"nkwagala", "Nkwagala|Spice Diana"},
	{"love nyo", "Love Nyo|Fik Fameica"},
	{"nasanga", "Nassanga (Remix)|Kalifah AgaNaga"},
	{"tuliyambala engule", "Tuliyambala Engule|Bobi Wine"},
	{"kafero walumbe", "Walumbe Zaaya|Paulo Kafeero"},
	{"bobi wine", "Tuliyambala Engule|Bobi Wine"},
	{"sauti sol", "Suzanna|Sauti Sol"},
	{"mbilo", "Mbilo Mbilo|Eddy Kenzo"},
	// popularity: a short or partial query means the hit, not an obscure
	// track that happens to match it more literally
	{"kenzo", "Sitya Loss|Eddy Kenzo"},
	{"essence", "Essence|Wizkid"},
	{"up", "Way Up|Lil HBK"},
	{"love", "Love Nyo|Fik Fameica"},
	{"loss", "Sitya Loss|Eddy Kenzo"},
	{"rema", "Calm Down|Rema"},
	// typos and spacing
	{"burnaboy", "Last Last|Burna Boy"},
	{"wiz kid", "Essence|Wizkid"},
	{"diamond platinumz", "Waah|Diamond Platnumz"},
	{"calm dwn", "Calm Down|Rema"},
	{"selena gomez calm down", "Calm Down (Remix)|Rema"},
	{"harmonise", "Kwangwaru|Harmonize"},
}

// evalSearch builds the catalog in a throwaway schema and returns a ready
// SearchService (nil when TEST_POSTGRES_DSN is unset).
func evalSearch(t *testing.T) *SearchService {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	const schema = "catalog_eval_test"
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
	in := &Ingester{DB: db, Priority: []string{"p0", "p1", "p2"}}
	var tracks []Track
	for i, s := range evalCatalog {
		for p := range s.sources {
			plays := s.plays / s.sources
			tracks = append(tracks, Track{Provider: []string{"p0", "p1", "p2"}[p], ExternalID: strings.Repeat("x", p+1) + string(rune('a'+i)),
				Title: s.title, Artist: s.artist, AudioURL: "u", Plays: &plays})
		}
	}
	if _, err := in.Ingest(tracks, t0); err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE INDEX ON catalog_songs USING gin (search_text gin_trgm_ops)`)
	svc := &SearchService{DB: db, Ingester: in, Fuzzy: true}
	if err := svc.PrepareSearchText(context.Background()); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestSearchQuality(t *testing.T) {
	runEval(t, evalSearch(t))
}

// The same eval with the in-memory index choosing the candidates.
func TestSearchQualityInMemory(t *testing.T) {
	svc := evalSearch(t)
	svc.Mem = &MemIndex{DB: svc.DB}
	if err := svc.Mem.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !svc.Mem.Ready() || svc.Mem.Size() == 0 {
		t.Fatal("in-memory index empty")
	}
	runEval(t, svc)
}

func runEval(t *testing.T, svc *SearchService) {
	t.Helper()
	hits := 0
	for _, c := range evalQueries {
		res, err := svc.Search(context.Background(), Query{Q: c.q, Limit: 5, Live: LiveOff})
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		got := "(nothing)"
		if len(res.Songs) > 0 {
			got = res.Songs[0].Title + "|" + res.Songs[0].Artist
		}
		if got == c.want {
			hits++
			continue
		}
		var top []string
		for _, s := range res.Songs[:min(3, len(res.Songs))] {
			top = append(top, s.Title+" / "+s.Artist)
		}
		t.Logf("MISS %-20q want %-40s got %v", c.q, c.want, top)
	}
	t.Logf("hit@1: %d/%d", hits, len(evalQueries))
	// a close match says what it understood
	if res, _ := svc.Search(context.Background(), Query{Q: "essense wizkid", Limit: 5, Live: LiveOff}); res.Suggest != "essence wizkid" {
		t.Errorf("did you mean: %q", res.Suggest)
	}
	if hits != len(evalQueries) {
		t.Errorf("hit@1 %d/%d (every query must find its song first)", hits, len(evalQueries))
	}
}

// Search as you type: the hit for a prefix comes first.
func TestCompleteQuality(t *testing.T) {
	svc := evalSearch(t)
	cases := map[string]string{
		"calm d":    "Calm Down",
		"cal":       "Calm Down",
		"sity":      "Sitya Loss",
		"eddy k":    "Sitya Loss",
		"burna":     "Last Last",
		"tuliya":    "Tuliyambala Engule",
		"dole yomw": "Dole Y'omwaana",
		"kwang":     "Kwangwaru",
	}
	for in, want := range cases {
		got, err := svc.Complete(context.Background(), in, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || got[0].Title != want {
			t.Errorf("%q: got %v, want %q first", in, got, want)
		}
	}
	if got, _ := svc.Complete(context.Background(), "c", 5); len(got) != 0 {
		t.Errorf("one letter: %v", got)
	}
}
