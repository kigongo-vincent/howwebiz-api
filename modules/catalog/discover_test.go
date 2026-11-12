package catalog

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

func seedCatalog(t *testing.T) *SearchService {
	t.Helper()
	db := testDB(t)
	in := &Ingester{DB: db}
	now := time.Now().UTC()
	var tracks []Track
	add := func(provider, id, title, artist, genre string, plays int, age time.Duration) {
		tracks = append(tracks, Track{Provider: provider, ExternalID: id, Title: title, Artist: artist, Genre: genre,
			AudioURL: "https://x/" + id + ".mp3", Plays: ip(plays), DurationMs: 200000, UpdatedAt: now.Add(-age)})
	}
	// the seed song, its remix and the artist's other songs
	add("howwe", "1", "Sitya Loss", "Eddy Kenzo", "AfroBeat", 900000, 0)
	add("howwe", "2", "Sitya Loss (Remix)", "Eddy Kenzo", "AfroBeat", 50000, 0)
	for i := range 6 {
		add("howwe", fmt.Sprint(10+i), fmt.Sprintf("Kenzo Song %d", i), "Eddy Kenzo", "AfroBeat", 10000, 0)
	}
	// a collaborator, same genre and region
	add("howwe", "20", "Tubonge", "Niniola ft. Eddy Kenzo", "AfroBeat", 20000, 0)
	// same genre, other artists
	for i := range 20 {
		add("ugamusic", fmt.Sprint(30+i), fmt.Sprintf("Afro Tune %d", i), fmt.Sprintf("Artist %d", i), "AfroBeat", 1000*(i+1), 0)
	}
	// other genres and regions
	for i := range 20 {
		add("fma", fmt.Sprint(60+i), fmt.Sprintf("Jazz Piece %d", i), fmt.Sprintf("Jazzer %d", i), "Jazz", 100, 0)
	}
	add("mdundo", "90", "Mwanza", "Tz Star", "Bongo Flava", 300000, 0)
	if _, err := in.Ingest(tracks, now); err != nil {
		t.Fatal(err)
	}
	return &SearchService{DB: db, Ingester: in}
}

func TestDiscoverIsShuffledStableAndSpread(t *testing.T) {
	s := seedCatalog(t)
	ctx := context.Background()
	page := func(seed uint64, p int) []Song {
		songs, total, err := s.Discover(ctx, DiscoverQuery{Kind: KindSong, Seed: seed, Page: p, Limit: 10})
		if err != nil || total == 0 {
			t.Fatalf("discover: %v (total %d)", err, total)
		}
		return songs
	}
	a1, a2 := page(1, 1), page(1, 2)
	if b1 := page(1, 1); !slices.EqualFunc(a1, b1, func(x, y Song) bool { return x.ID == y.ID }) {
		t.Fatal("the same seed must give the same page")
	}
	for _, x := range a1 {
		if slices.ContainsFunc(a2, func(y Song) bool { return y.ID == x.ID }) {
			t.Fatalf("pages overlap on %d", x.ID)
		}
	}
	differs := false
	for seed := uint64(2); seed < 6 && !differs; seed++ {
		c := page(seed, 1)
		differs = !slices.EqualFunc(a1, c, func(x, y Song) bool { return x.ID == y.ID })
	}
	if !differs {
		t.Fatal("other seeds should shuffle differently")
	}
	// no artist twice within three places
	for i := range a1 {
		for j := max(0, i-2); j < i; j++ {
			if artistKey(a1[i]) == artistKey(a1[j]) {
				t.Fatalf("artist %q repeats at %d and %d", artistKey(a1[i]), j, i)
			}
		}
	}
	if len(a1[0].Sources) == 0 {
		t.Fatal("sources not loaded")
	}
}

func TestDiscoverRegionFavoursRegion(t *testing.T) {
	s := seedCatalog(t)
	tz := 0
	for seed := range uint64(20) {
		songs, _, err := s.Discover(context.Background(), DiscoverQuery{Kind: KindSong, Region: "tz", Seed: seed, Page: 1, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range songs {
			if x.Artist == "Tz Star" {
				tz++
			}
		}
	}
	if tz < 10 {
		t.Fatalf("the one Tanzanian song made the first page %d/20 times", tz)
	}
}

func TestNextRecommendsSimilarNotVersions(t *testing.T) {
	s := seedCatalog(t)
	var seed Song
	s.DB.Where("title = ?", "Sitya Loss").First(&seed)
	_, songs, err := s.Next(context.Background(), seed.ID, NextQuery{Seed: 1, Limit: 15})
	if err != nil {
		t.Fatal(err)
	}
	own, afro, collab := 0, 0, false
	for _, x := range songs {
		if x.ID == seed.ID || baseTitle(x.Title) == "sitya loss" {
			t.Fatalf("suggested %q: the same song", x.Title)
		}
		if artistKey(x) == "eddy kenzo" {
			own++
		}
		if x.Genre == "AfroBeat" {
			afro++
		}
		collab = collab || x.Title == "Tubonge"
	}
	if own > 2 {
		t.Fatalf("%d songs by the same artist", own)
	}
	if afro < len(songs)*2/3 {
		t.Fatalf("only %d/%d in the seed's genre", afro, len(songs))
	}
	if !collab {
		t.Fatal("the collaborator's song should be suggested")
	}
	// played songs are not suggested again
	_, again, _ := s.Next(context.Background(), seed.ID, NextQuery{Seed: 1, Limit: 15, Exclude: []uint{songs[0].ID}})
	if slices.ContainsFunc(again, func(x Song) bool { return x.ID == songs[0].ID }) {
		t.Fatal("excluded song suggested")
	}
}

func TestBaseTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Sitya Loss (Remix)": "sitya loss", "Sugar - Official Audio": "sugar", "Love Song": "love song",
		"Banana [Remix]": "banana", "Way Up Live Acoustic": "way up", "Stand by Me": "stand by me",
	} {
		if got := baseTitle(in); got != want {
			t.Errorf("baseTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRelevanceBoundedCandidatesKeepExactMatch(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC()
	songs := make([]Song, 0, candidateCap+300)
	for i := range candidateCap + 300 { // more broad matches than one lookup takes
		title := fmt.Sprintf("King Tune %d", i)
		key := fmt.Sprintf("artist %d|king tune %d", i, i)
		songs = append(songs, Song{Key: key, Title: title, Artist: fmt.Sprintf("Artist %d", i), Kind: KindSong,
			AudioURL: "https://x/a.mp3", Plays: int64(1000 + i), FirstSeenAt: now, UpdatedAt: now,
			SearchText: SearchText(title, fmt.Sprintf("Artist %d", i), nil, key), SearchVersion: SearchTextVersion})
	}
	exact := Song{Key: "king saha|vimba", Title: "Vimba", Artist: "King Saha", Kind: KindSong, AudioURL: "https://x/v.mp3",
		FirstSeenAt: now, UpdatedAt: now, SearchText: SearchText("Vimba", "King Saha", nil, "king saha|vimba"), SearchVersion: SearchTextVersion}
	songs = append(songs, exact)
	if err := db.CreateInBatches(songs, 200).Error; err != nil {
		t.Fatal(err)
	}
	s := &SearchService{DB: db, Ingester: &Ingester{DB: db}}
	s.textIndexed.Store(true)
	for _, q := range []string{"vimba king saha", "king saha vimba", "vimba"} {
		res, err := s.Search(context.Background(), Query{Q: q, Limit: 10, Live: LiveOff})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Songs) == 0 || res.Songs[0].Title != "Vimba" {
			t.Fatalf("%q: first result %v", q, res.Songs[:min(1, len(res.Songs))])
		}
	}
	// a broad single word is answered from a bounded set, still most played first
	res, err := s.Search(context.Background(), Query{Q: "king", Limit: 5, Live: LiveOff})
	if err != nil || len(res.Songs) != 5 || res.Total > int64(candidateCap+1) {
		t.Fatalf("broad: %v total %d", err, res.Total)
	}
}

func count(songs []Song, f func(Song) bool) int {
	n := 0
	for _, s := range songs {
		if f(s) {
			n++
		}
	}
	return n
}

func TestRecommendFromGenreAndArtistSeeds(t *testing.T) {
	s := seedCatalog(t)
	ctx := context.Background()
	jazz, err := s.Recommend(ctx, RecommendQuery{Genres: []string{"jazz"}, Seed: 3, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(jazz, func(x Song) bool { return x.Genre == "Jazz" }); n < 8 {
		t.Fatalf("genre seed jazz: %d/%d jazz", n, len(jazz))
	}
	// an artist seed brings the artist's world (genre, collaborators), not
	// only the artist: at most two of theirs
	kenzo, err := s.Recommend(ctx, RecommendQuery{Artists: []string{"Eddy Kenzo"}, Seed: 3, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(kenzo, func(x Song) bool { return artistKey(x) == "eddy kenzo" }); n > 2 {
		t.Fatalf("artist seed: %d of the artist's own songs", n)
	}
	if n := count(kenzo, func(x Song) bool { return x.Title == "Tubonge" }); n != 1 {
		t.Fatal("the collaborator's song should come up for an artist seed")
	}
}

func TestNextFollowsTheSessionNotJustTheSong(t *testing.T) {
	s := seedCatalog(t)
	ctx := context.Background()
	var playing Song
	s.DB.Where("title = ?", "Afro Tune 3").First(&playing)
	var jazzIDs []uint
	s.DB.Model(&Song{}).Where("genre = ?", "Jazz").Limit(5).Pluck("id", &jazzIDs)
	jazzOf := func(h []uint) int {
		_, songs, err := s.Next(ctx, playing.ID, NextQuery{Seed: 9, Limit: 12, History: h})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range songs {
			if slices.Contains(h, x.ID) {
				t.Fatalf("history song %d suggested again", x.ID)
			}
		}
		return count(songs, func(x Song) bool { return x.Genre == "Jazz" })
	}
	alone, withHistory := jazzOf(nil), jazzOf(jazzIDs)
	if withHistory <= alone {
		t.Fatalf("a jazz session should bring jazz: %d alone, %d with history", alone, withHistory)
	}
}

func TestRecommendPopularityTarget(t *testing.T) {
	s := seedCatalog(t)
	avg := func(target float64) float64 {
		songs, err := s.Recommend(context.Background(), RecommendQuery{Genres: []string{"AfroBeat"}, Popularity: &target, Seed: 5, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		sum := 0.0
		for _, x := range songs {
			sum += popularity01(x.Plays)
		}
		return sum / float64(len(songs))
	}
	if hits, deep := avg(100), avg(0); hits <= deep {
		t.Fatalf("popularity=100 should lean to hits: %.2f vs %.2f", hits, deep)
	}
}
