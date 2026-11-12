package catalog

// Discovery: a shuffled feed (sort=discover) and "up next" recommendations
// (/songs/:id/next).
//
// Both read a few small candidate pools, each one indexed lookup (recent,
// most played, a genre's top, a random stretch of the catalog, an artist's
// songs), and do the rest in Go: score each candidate, then a weighted
// shuffle (Efraimidis-Spirakis: key = u^(1/w)), so better songs come up more
// often without the order ever being fixed, then spread artists out. A seed
// makes a shuffle repeatable, so pages of one feed never overlap.

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

// Regions a client can ask for (?region=).
var Regions = []string{"ug", "ke", "tz", "rw", "ng", "gh", "za", "africa", "global"}

// providerRegion: where a provider's catalog comes from.
var providerRegion = map[string]string{
	"howwe": "ug", "ugamusic": "ug", "pearltunes": "ug", "momuziki": "ug",
	"mzikii": "ug", "ugmuziki": "ug", "busoga": "ug",
	"mdundo": "africa",
}

// genreRegion: genres that place a song in a country.
var genreRegion = map[string]string{
	"bongo flava": "tz", "singeli": "tz", "taarab": "tz", "rhumba zilizopendwa": "tz",
	"kapuka": "ke", "genge": "ke", "benga": "ke", "gengetone": "ke",
	"highlife": "gh", "amapiano": "za", "kwaito": "za", "gqom": "za",
	"kadongo kamu": "ug", "afro vernacular": "ug", "luga flow": "ug",
	"afrobeats": "ng", "naija": "ng", "fuji": "ng",
}

var african = map[string]bool{"ug": true, "ke": true, "tz": true, "rw": true, "ng": true, "gh": true, "za": true, "africa": true}

// songRegions: every region a song belongs to.
func songRegions(s Song) map[string]bool {
	out := map[string]bool{}
	for _, p := range s.Providers {
		if r, ok := providerRegion[p]; ok {
			out[r] = true
		}
	}
	if r, ok := genreRegion[strings.ToLower(strings.TrimSpace(s.Genre))]; ok {
		out[r] = true
	}
	for r := range out {
		if african[r] {
			out["africa"] = true
		}
	}
	if len(out) == 0 {
		out["global"] = true
	}
	return out
}

// regionGenres: genres placing a song in region (for a candidate pool).
func regionGenres(region string) []string {
	var out []string
	for g, r := range genreRegion {
		if r == region {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

func regionProviders(region string) []string {
	var out []string
	for p, r := range providerRegion {
		if r == region || (region == "africa" && african[r]) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// popularity01: plays on a log scale, 0..1 (1M plays and more = 1).
func popularity01(plays int64) float64 {
	if plays <= 0 {
		return 0
	}
	return math.Min(1, math.Log10(float64(plays)+1)/6)
}

func spread01(sources int) float64 { return math.Min(float64(max(sources-1, 0)), 3) / 3 }

// when: the song's release, or when the catalog first saw it.
func when(s Song, now time.Time) time.Time {
	if s.ReleasedAt != nil && !s.ReleasedAt.IsZero() && s.ReleasedAt.Before(now) {
		return *s.ReleasedAt
	}
	return s.FirstSeenAt
}

// freshness01: 1 for a song out today, halving about every three weeks.
func freshness01(s Song, now time.Time) float64 {
	days := now.Sub(when(s, now)).Hours() / 24
	return math.Exp(-math.Max(days, 0) / 30)
}

// versionWords mark another version of one song (not a different song).
var versionWords = map[string]bool{"remix": true, "rmx": true, "version": true, "live": true, "acoustic": true,
	"instrumental": true, "cover": true, "edit": true, "extended": true, "slowed": true, "reverb": true,
	"sped": true, "audio": true, "official": true, "video": true, "lyrics": true,
	"mix": true, "radio": true, "clean": true, "explicit": true, "prod": true}

// baseTitle: the title without version markers ("Sugar (Remix)" = "sugar").
func baseTitle(title string) string {
	if i := strings.IndexAny(title, "([{"); i > 0 {
		title = title[:i]
	}
	var out []string
	for _, w := range strings.Fields(Fold(title)) {
		if !versionWords[w] {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

var stopWords = map[string]bool{"the": true, "and": true, "you": true, "your": true, "with": true, "for": true,
	"that": true, "this": true, "from": true, "feat": true, "featuring": true, "part": true, "vol": true}

// titleTokens: the informative words of a title (language, theme: "yesu",
// "katonda", "love", "gospel" tie songs together across artists).
func titleTokens(title string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(Fold(title)) {
		if utf8.RuneCountInString(w) >= 4 && !stopWords[w] && !versionWords[w] {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// genreClose: same genre, or one spelled differently ("Afro Beat" and
// "Afrobeats", "R&B" and "R-B-Soul").
func genreClose(a, b string) float64 {
	fa, fb := strings.ReplaceAll(Fold(a), " ", ""), strings.ReplaceAll(Fold(b), " ", "")
	switch {
	case fa == "" || fb == "":
		return 0
	case fa == fb:
		return 1
	case len(fa) >= 4 && len(fb) >= 4 && (strings.HasPrefix(fa, fb[:4]) || strings.HasPrefix(fb, fa[:4])):
		return 0.6
	}
	return 0
}

func artistKey(s Song) string {
	if i := strings.IndexByte(s.Key, '|'); i > 0 {
		return s.Key[:i]
	}
	return Fold(s.Artist)
}

// SeedFor derives a shuffle seed from text: the same text, the same order.
func SeedFor(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

type scored struct {
	song Song
	w    float64
}

// cachedScored is scored as stored in Redis (Song.Key isn't serialised).
type cachedScored struct {
	Song Song    `json:"s"`
	Key  string  `json:"k"`
	W    float64 `json:"w"`
}

func toCache(items []scored) []cachedScored {
	out := make([]cachedScored, len(items))
	for i, it := range items {
		out[i] = cachedScored{it.song, it.song.Key, it.w}
	}
	return out
}

func fromCache(items []cachedScored) []scored {
	out := make([]scored, len(items))
	for i, it := range items {
		it.Song.Key = it.Key
		out[i] = scored{it.Song, it.W}
	}
	return out
}

// poolTTL: how long a scored candidate pool is reused. Building one is a
// handful of queries; shuffling it is free, so every request can get its
// own order without touching the database.
const poolTTL = 5 * time.Minute

// weightedShuffle orders songs at random, each one's chance of an early
// place growing with its weight.
func weightedShuffle(items []scored, seed uint64) []Song {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	type keyed struct {
		s Song
		k float64
	}
	ks := make([]keyed, len(items))
	for i, it := range items {
		w := math.Max(it.w, 1e-6)
		ks[i] = keyed{it.song, math.Pow(r.Float64(), 1/w)}
	}
	slices.SortStableFunc(ks, func(a, b keyed) int {
		switch {
		case a.k > b.k:
			return -1
		case a.k < b.k:
			return 1
		}
		return int(b.s.ID) - int(a.s.ID)
	})
	out := make([]Song, len(ks))
	for i, k := range ks {
		out[i] = k.s
	}
	return out
}

// spreadArtists reorders songs so no artist plays twice within gap places
// and none has more than perArtist songs in any window of window places
// (songs that don't fit yet wait for a later place; leftovers go last).
func spreadArtists(songs []Song, gap, perArtist, window int) []Song {
	out := make([]Song, 0, len(songs))
	waiting := slices.Clone(songs)
	fits := func(s Song) bool {
		a := artistKey(s)
		n := 0
		for i := len(out) - 1; i >= 0 && i >= len(out)-window; i-- {
			if artistKey(out[i]) == a {
				if i >= len(out)-gap {
					return false
				}
				n++
			}
		}
		return n < perArtist
	}
	for len(waiting) > 0 {
		placed := false
		for i, s := range waiting {
			if fits(s) {
				out = append(out, s)
				waiting = slices.Delete(waiting, i, i+1)
				placed = true
				break
			}
		}
		if !placed { // only crowded artists left
			out = append(out, waiting...)
			break
		}
	}
	return out
}

// candidate columns: everything scoring needs, not search_text.
var poolColumns = []string{"id", "key", "title", "artist", "featured", "genre", "kind", "cover_url", "audio_url",
	"duration_ms", "plays", "providers", "source_count", "released_at", "first_seen_at", "updated_at"}

type pools struct {
	db   *gorm.DB
	seen map[uint]bool
	all  []Song
	err  error
}

// add runs one pool query (base filters already applied by the caller).
func (p *pools) add(q *gorm.DB) {
	if p.err != nil {
		return
	}
	var rows []Song
	if err := q.Select(poolColumns).Find(&rows).Error; err != nil {
		p.err = err
		return
	}
	for _, r := range rows {
		if !p.seen[r.ID] {
			p.seen[r.ID] = true
			p.all = append(p.all, r)
		}
	}
}

// DiscoverQuery: the shuffled feed.
type DiscoverQuery struct {
	Genre, Artist, Region, Kind string
	Seed                        uint64
	Page, Limit                 int
}

// discoverPool caps the songs one feed shuffles (pages past it are empty).
const discoverPool = 600

// Discover returns one page of a shuffled feed and its size. Popular,
// widely carried, fresh and in-region songs come up more often; every page
// mixes artists; the same seed gives the same feed.
func (s *SearchService) Discover(ctx context.Context, q DiscoverQuery) ([]Song, int64, error) {
	now := time.Now().UTC()
	key := "discover:" + s.Cache.Generation(ctx, cacheNS) + ":" + cacheKey(q.Genre, q.Artist, q.Region, q.Kind)
	var cached []cachedScored
	var items []scored
	if s.Cache.Get(ctx, key, &cached) {
		items = fromCache(cached)
	} else {
		var err error
		if items, err = s.discoverPool(ctx, q, now); err != nil {
			return nil, 0, err
		}
		s.Cache.Set(ctx, key, toCache(items), poolTTL)
	}
	ordered := weightedShuffle(items, q.Seed)
	if q.Artist == "" {
		ordered = spreadArtists(ordered, 3, 2, 20)
	}
	if len(ordered) > discoverPool {
		ordered = ordered[:discoverPool]
	}
	// The feed is the shuffled pool, then the rest of the catalog newest
	// first: its total is the catalog's, and scrolling never runs dry.
	total := max(int64(len(ordered)), s.catalogTotal(ctx, q))
	from := (q.Page - 1) * q.Limit
	if from < len(ordered) {
		page := ordered[from:min(from+q.Limit, len(ordered))]
		songs, _, err := s.withSources(ctx, page)
		return songs, total, err
	}
	pool := make([]uint, len(ordered))
	for i, c := range ordered {
		pool[i] = c.ID
	}
	db := s.discoverBase(ctx, q).Order("first_seen_at DESC").Order("id DESC").Offset(from - len(ordered)).Limit(q.Limit)
	if len(pool) > 0 {
		db = db.Where("id NOT IN ?", pool)
	}
	var rest []Song
	if err := db.Select(poolColumns).Find(&rest).Error; err != nil {
		return nil, total, err
	}
	songs, _, err := s.withSources(ctx, rest)
	return songs, total, err
}

// catalogTotal: how many songs the feed's filters match (the totals
// RefreshTotals keeps, else the last count a list made, else the
// planner's estimate for the whole catalog; 0 when unknown).
func (s *SearchService) catalogTotal(ctx context.Context, q DiscoverQuery) int64 {
	var n int64
	filters := []any{"", Fold(q.Artist), strings.ToLower(q.Genre), "", "", q.Kind}
	if s.Cache.Get(ctx, "count-last:"+cacheKey(filters...), &n) {
		return n
	}
	if q.Artist == "" && q.Genre == "" && s.estimate(ctx, &n) {
		return n
	}
	return 0
}

// discoverBase: the feed's filters.
func (s *SearchService) discoverBase(ctx context.Context, q DiscoverQuery) *gorm.DB {
	db := s.DB.WithContext(ctx).Model(&Song{}).Where("audio_url <> ''")
	if q.Kind != "all" {
		db = db.Where("kind = ?", q.Kind)
	}
	if q.Genre != "" {
		db = db.Where("LOWER(genre) = ?", strings.ToLower(q.Genre))
	}
	if q.Artist != "" {
		db = db.Where("key LIKE ? ESCAPE '\\'", likeEscape(Fold(q.Artist))+"|%")
	}
	return db
}

// discoverPool reads and scores a feed's candidates (see Discover).
func (s *SearchService) discoverPool(ctx context.Context, q DiscoverQuery, now time.Time) ([]scored, error) {
	base := func() *gorm.DB { return s.discoverBase(ctx, q) }
	p := &pools{seen: map[uint]bool{}}
	p.add(base().Order("plays DESC").Order("id DESC").Limit(200))         // the hits
	p.add(base().Order("first_seen_at DESC").Order("id DESC").Limit(200)) // what's new
	if q.Genre == "" && q.Artist == "" {
		// the long tail: a random stretch of the catalog (a primary-key range)
		var maxID uint
		if err := s.DB.WithContext(ctx).Model(&Song{}).Select("COALESCE(MAX(id), 0)").Scan(&maxID).Error; err == nil && maxID > 0 {
			// a new stretch every pool (not per seed: the pool is shared)
			r := rand.New(rand.NewPCG(uint64(now.UnixNano()), 7))
			from := uint(r.Uint64N(uint64(maxID))) + 1
			p.add(base().Where("id >= ?", from).Order("id").Limit(120))
		}
	}
	if q.Region != "" && q.Region != "global" {
		if gs := regionGenres(q.Region); len(gs) > 0 && q.Genre == "" {
			p.add(base().Where("LOWER(genre) IN ?", gs).Order("plays DESC").Order("id DESC").Limit(150))
		}
		if ps := regionProviders(q.Region); len(ps) > 0 {
			ids := s.DB.WithContext(ctx).Model(&Source{}).Select("song_id").Where("provider IN ?", ps).Order("id DESC").Limit(200)
			p.add(base().Where("id IN (?)", ids))
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	items := make([]scored, 0, len(p.all))
	for _, c := range p.all {
		w := 0.15 + 0.35*popularity01(c.Plays) + 0.2*spread01(c.SourceCount) + 0.3*freshness01(c, now)
		if q.Region != "" && songRegions(c)[q.Region] {
			w += 0.5
		}
		items = append(items, scored{c, w})
	}
	return items, nil
}

// withSources loads the sources of a page of songs, keeping its order.
func (s *SearchService) withSources(ctx context.Context, page []Song) ([]Song, int64, error) {
	if len(page) == 0 {
		return []Song{}, 0, nil
	}
	ids := make([]uint, len(page))
	for i, p := range page {
		ids[i] = p.ID
	}
	var srcs []Source
	if err := s.DB.WithContext(ctx).Where("song_id IN ?", ids).Find(&srcs).Error; err != nil {
		return nil, 0, err
	}
	by := map[uint][]Source{}
	for _, src := range srcs {
		by[src.SongID] = append(by[src.SongID], src)
	}
	out := slices.Clone(page)
	for i := range out {
		out[i].Sources = by[out[i].ID]
		if out[i].Sources == nil {
			out[i].Sources = []Source{}
		}
	}
	return out, int64(len(page)), nil
}
