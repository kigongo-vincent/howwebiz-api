package catalog

// Recommendations from a taste profile, the way streaming services build
// radio and "up next": not "songs whose name looks like this one", but
// songs that fit what is being listened to.
//
// A profile is built from seeds: songs (by id: the song playing, the
// recent history), artists and genres. It holds
//   - a genre mix (weighted: the song playing counts most),
//   - artists, and their collaborators (who they feature, who features them),
//   - regions, an era (when the seeds came out) and a typical reach (plays),
//   - title themes (shared words: language, subject: "yesu", "love"),
//   - typical length.
//
// Candidates come from small indexed pools around the profile (each
// genre's top songs, each artist's and collaborator's songs, the era, the
// region, what's fresh). Each is scored against the whole profile, then
// shuffled by weight (a seed makes it repeatable), artists spread out, and
// versions of a seed song (remix, live, cover...) left out.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
)

// RecommendQuery: what to recommend from. At least one seed.
type RecommendQuery struct {
	// Songs: seed song ids, most important first (for "up next": the song
	// playing, then the recent history, newest first).
	Songs []uint
	// Artists, Genres: seeds by name (onboarding: "I like Azawi, Afrobeat").
	Artists, Genres []string
	Region          string // favour a region ("" = the seeds' own)
	// MinYear, MaxYear: only songs released (or first seen) in these years.
	MinYear, MaxYear int
	// Popularity: 0-100 target reach (nil = the seeds' own). 100 = hits only.
	Popularity *float64
	// Exclude: never suggest these (already played, skipped). Seed songs
	// are excluded too.
	Exclude []uint
	Kind    string // "" = the first seed song's kind, else song
	Seed    uint64
	Limit   int
}

type profile struct {
	genres    map[string]float64 // folded, no spaces -> weight
	genreRaw  map[string]string  // folded -> as stored (for pool queries)
	artists   map[string]float64 // artist keys -> weight
	collab    map[string]bool
	regions   map[string]bool
	era       time.Time
	eraSpread float64 // days
	pop       float64 // 0..1
	popAsked  bool    // pop was asked for (?popularity=): weighs as much as genre
	tokens    map[string]bool
	duration  int
	bases     map[string]bool // base titles of the seed songs (versions excluded)
}

func genreKey(g string) string { return strings.ReplaceAll(Fold(g), " ", "") }

// genreAffinity: how well a candidate's genre fits the profile's mix.
func (p profile) genreAffinity(g string) float64 {
	k := genreKey(g)
	if k == "" || len(p.genres) == 0 {
		return 0
	}
	best := p.genres[k]
	for pg, w := range p.genres {
		if c := genreClose(pg, k); c > 0 && c*w > best {
			best = c * w
		}
	}
	return best
}

func (s *SearchService) buildProfile(ctx context.Context, q RecommendQuery, now time.Time) (profile, []Song, error) {
	p := profile{genres: map[string]float64{}, genreRaw: map[string]string{}, artists: map[string]float64{},
		collab: map[string]bool{}, regions: map[string]bool{}, tokens: map[string]bool{}, bases: map[string]bool{}}
	var seeds []Song
	if len(q.Songs) > 0 {
		if err := s.DB.WithContext(ctx).Select(poolColumns).Where("id IN ?", q.Songs).Find(&seeds).Error; err != nil {
			return p, nil, err
		}
		// keep the caller's order: the first seed matters most
		pos := map[uint]int{}
		for i, id := range q.Songs {
			if _, ok := pos[id]; !ok {
				pos[id] = i
			}
		}
		slices.SortFunc(seeds, func(a, b Song) int { return pos[a.ID] - pos[b.ID] })
	}
	var dates []float64
	popSum, popN, durSum, durN := 0.0, 0, 0, 0
	for i, sd := range seeds {
		w := 1 / (1 + 0.35*float64(i)) // the song playing counts most, older history less
		if sd.Genre != "" {
			k := genreKey(sd.Genre)
			p.genres[k] += w
			p.genreRaw[k] = sd.Genre
		}
		a := artistKey(sd)
		p.artists[a] += w
		for _, f := range sd.Featured {
			if k := Fold(f); k != "" {
				p.collab[k] = true
			}
		}
		for r := range songRegions(sd) {
			p.regions[r] = true
		}
		for t := range titleTokens(sd.Title) {
			p.tokens[t] = true
		}
		if b := baseTitle(sd.Title); b != "" {
			p.bases[b] = true
		}
		dates = append(dates, float64(when(sd, now).Unix()))
		popSum += popularity01(sd.Plays)
		popN++
		if sd.DurationMs > 0 {
			durSum += sd.DurationMs
			durN++
		}
	}
	for _, g := range q.Genres {
		if k := genreKey(g); k != "" {
			p.genres[k] += 1
			p.genreRaw[k] = g
		}
	}
	for _, a := range q.Artists {
		if k := Fold(a); k != "" {
			p.artists[k] += 1
		}
	}
	// normalise the genre mix to a max of 1
	maxW := 0.0
	for _, w := range p.genres {
		maxW = math.Max(maxW, w)
	}
	for k := range p.genres {
		p.genres[k] /= maxW
	}
	if q.Region != "" {
		p.regions = map[string]bool{q.Region: true}
	}
	if len(dates) > 0 {
		slices.Sort(dates)
		p.era = time.Unix(int64(dates[len(dates)/2]), 0).UTC()
		spread := (dates[len(dates)-1] - dates[0]) / 86400
		p.eraSpread = math.Max(180, spread)
	} else {
		p.era, p.eraSpread = now, 365*3
	}
	switch {
	case q.Popularity != nil:
		p.pop = math.Min(1, math.Max(0, *q.Popularity/100))
		p.popAsked = true
	case popN > 0:
		p.pop = popSum / float64(popN)
	default:
		p.pop = 0.5
	}
	if durN > 0 {
		p.duration = durSum / durN
	}
	return p, seeds, nil
}

// recPool: a scored candidate pool and the seed artists (cached).
type recPool struct {
	Items   []cachedScored     `json:"i"`
	Artists map[string]float64 `json:"a"`
}

// Recommend returns songs that fit the seeds' taste profile, best first.
// The scored pool is cached for poolTTL; each call shuffles it with its
// own seed, so every request can get a different list for free.
func (s *SearchService) Recommend(ctx context.Context, q RecommendQuery) ([]Song, error) {
	pop := ""
	if q.Popularity != nil {
		pop = fmt.Sprint(*q.Popularity)
	}
	key := "recommend:" + s.Cache.Generation(ctx, cacheNS) + ":" + cacheKey(fmt.Sprint(q.Songs), fmt.Sprint(q.Artists),
		fmt.Sprint(q.Genres), q.Region, q.MinYear, q.MaxYear, pop, fmt.Sprint(q.Exclude), q.Kind)
	var rp recPool
	if !s.Cache.Get(ctx, key, &rp) {
		items, artists, err := s.recommendPool(ctx, q)
		if err != nil {
			return nil, err
		}
		rp = recPool{toCache(items), artists}
		s.Cache.Set(ctx, key, rp, poolTTL)
	}
	ordered := spreadArtists(weightedShuffle(fromCache(rp.Items), q.Seed), 4, 1, 10)
	// a seed artist: at most two songs in a list
	var out []Song
	perSeedArtist := map[string]int{}
	for _, c := range ordered {
		a := artistKey(c)
		if rp.Artists[a] > 0 {
			if perSeedArtist[a] == 2 {
				continue
			}
			perSeedArtist[a]++
		}
		out = append(out, c)
		if len(out) == q.Limit {
			break
		}
	}
	songs, _, err := s.withSources(ctx, out)
	return songs, err
}

// recommendPool builds the profile, reads the candidate pools and scores them.
func (s *SearchService) recommendPool(ctx context.Context, q RecommendQuery) ([]scored, map[string]float64, error) {
	now := time.Now().UTC()
	p, seeds, err := s.buildProfile(ctx, q, now)
	if err != nil {
		return nil, nil, err
	}
	if len(seeds) == 0 && len(q.Songs) > 0 && len(q.Artists) == 0 && len(q.Genres) == 0 {
		return nil, nil, gorm.ErrRecordNotFound
	}
	kind := q.Kind
	if kind == "" {
		kind = KindSong
		if len(seeds) > 0 {
			kind = seeds[0].Kind
		}
	}
	excluded := map[uint]bool{}
	for _, x := range q.Exclude {
		excluded[x] = true
	}
	for _, sd := range seeds {
		excluded[sd.ID] = true
	}
	base := func() *gorm.DB {
		db := s.DB.WithContext(ctx).Model(&Song{}).Where("audio_url <> '' AND kind = ?", kind)
		if q.MinYear > 0 {
			db = db.Where("COALESCE(released_at, first_seen_at) >= ?", time.Date(q.MinYear, 1, 1, 0, 0, 0, 0, time.UTC))
		}
		if q.MaxYear > 0 {
			db = db.Where("COALESCE(released_at, first_seen_at) < ?", time.Date(q.MaxYear+1, 1, 1, 0, 0, 0, 0, time.UTC))
		}
		return db
	}
	pl := &pools{seen: map[uint]bool{}}
	// the genre mix: each genre's top songs, more from the main genres
	type gw struct {
		raw string
		w   float64
	}
	var gs []gw
	for k, w := range p.genres {
		gs = append(gs, gw{p.genreRaw[k], w})
	}
	slices.SortFunc(gs, func(a, b gw) int {
		if a.w != b.w {
			if a.w > b.w {
				return -1
			}
			return 1
		}
		return strings.Compare(a.raw, b.raw)
	})
	for i, g := range gs {
		if i == 4 {
			break
		}
		pl.add(base().Where("LOWER(genre) = ?", strings.ToLower(g.raw)).Order("plays DESC").Order("id DESC").Limit(int(60 + 120*g.w)))
	}
	// artists and their collaborators
	names := map[string]bool{}
	for a := range p.artists {
		names[a] = true
	}
	for c := range p.collab {
		names[c] = true
	}
	sortedNames := make([]string, 0, len(names))
	for n := range names {
		sortedNames = append(sortedNames, n)
	}
	slices.Sort(sortedNames)
	for i, n := range sortedNames {
		if i == 8 {
			break
		}
		pl.add(base().Where("key LIKE ? ESCAPE '\\'", likeEscape(n)+"|%").Order("plays DESC").Limit(20))
	}
	// the era, the region, and what's fresh
	pl.add(base().Where("first_seen_at BETWEEN ? AND ?", p.era.AddDate(0, -3, 0), p.era.AddDate(0, 3, 0)).
		Order("first_seen_at DESC").Order("id DESC").Limit(100))
	for r := range p.regions {
		if ps := regionProviders(r); len(ps) > 0 {
			ids := s.DB.WithContext(ctx).Model(&Source{}).Select("song_id").Where("provider IN ?", ps).Order("id DESC").Limit(150)
			pl.add(base().Where("id IN (?)", ids))
		}
		if rg := regionGenres(r); len(rg) > 0 {
			pl.add(base().Where("LOWER(genre) IN ?", rg).Order("plays DESC").Order("id DESC").Limit(80))
		}
	}
	pl.add(base().Order("first_seen_at DESC").Order("id DESC").Limit(60))
	if pl.err != nil {
		return nil, nil, pl.err
	}

	items := make([]scored, 0, len(pl.all))
	for _, c := range pl.all {
		if excluded[c.ID] || p.bases[baseTitle(c.Title)] {
			continue // played, or another version of a seed song
		}
		items = append(items, scored{c, p.score(c, now)})
	}
	return items, p.artists, nil
}

// score: how well c fits the profile (squared: close songs clearly first).
func (p profile) score(c Song, now time.Time) float64 {
	a := artistKey(c)
	w := 0.05
	w += 1.6 * p.genreAffinity(c.Genre) // the strongest signal: what kind of music it is
	// the collaboration graph: an artist the seeds feature, an artist who
	// features a seed artist, or one sharing a guest with them
	shared := p.collab[a]
	for _, f := range c.Featured {
		k := Fold(f)
		shared = shared || p.artists[k] > 0 || p.collab[k]
	}
	if shared {
		w += 0.9
	}
	if p.artists[a] > 0 {
		w += 0.4 * math.Min(1, p.artists[a])
	}
	cr := songRegions(c)
	for r := range p.regions {
		if cr[r] {
			w += 0.5
			break
		}
	}
	days := math.Abs(when(c, now).Sub(p.era).Hours() / 24)
	w += 0.4 * math.Exp(-days/p.eraSpread) // same era
	reach := 0.35
	if p.popAsked {
		reach = 1.6
	}
	w += reach * (1 - math.Abs(popularity01(c.Plays)-p.pop)) // similar reach
	if p.duration > 0 && c.DurationMs > 0 {                  // similar length
		w += 0.1 * math.Exp(-math.Abs(float64(c.DurationMs-p.duration))/90000)
	}
	w += 0.3 * jaccard(p.tokens, titleTokens(c.Title)) // shared themes
	w += 0.2*popularity01(c.Plays) + 0.1*spread01(c.SourceCount) + 0.15*freshness01(c, now)
	if c.CoverURL == "" { // a bare entry plays worse in a player
		w -= 0.1
	}
	w = math.Max(w, 0.01)
	return w * w
}

// NextQuery: up next after a song.
type NextQuery struct {
	Region  string
	Seed    uint64
	Limit   int
	History []uint // recently played, newest first: refines the taste, never repeated
	Exclude []uint // never suggested (skipped, disliked)
}

// Next is Recommend seeded by the song playing (first, weighed most) and
// the recent history.
func (s *SearchService) Next(ctx context.Context, id uint, q NextQuery) (Song, []Song, error) {
	var seed Song
	if err := s.DB.WithContext(ctx).Select(poolColumns).First(&seed, id).Error; err != nil {
		return seed, nil, err
	}
	seeds := append([]uint{id}, q.History...)
	if len(seeds) > 10 { // the last ten plays shape the taste
		seeds = seeds[:10]
	}
	songs, err := s.Recommend(ctx, RecommendQuery{Songs: seeds, Region: q.Region, Seed: q.Seed, Limit: q.Limit,
		Exclude: append(slices.Clone(q.Exclude), q.History...), Kind: seed.Kind})
	return seed, songs, err
}
