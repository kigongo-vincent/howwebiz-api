package catalog

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/gorm"
)

// ProviderInfo describes a registered provider for /providers.
type ProviderInfo struct {
	Name        string `json:"name"`
	Site        string `json:"site"`
	Ingest      string `json:"ingest"` // "index" (own store, pulled by the indexer) | "push" (crawler writes here)
	LiveSearch  bool   `json:"liveSearch"`
	Description string `json:"description"`
}

// Service is the catalog's HTTP adapter.
type Service struct {
	DB        *gorm.DB
	Search    *SearchService
	Indexer   *Indexer
	Ingester  *Ingester
	Providers []ProviderInfo
}

var sorts = []string{"relevance", "recent", "plays", "title", "discover"}

var types = []string{KindSong, KindMix, "all"}

// List is the aggregated songs endpoint.
//
//	?q=         text (title, artist, featured artists; accent/punctuation-insensitive)
//	?artist=    primary artist
//	?genre=  ?provider=  ?hasAudio=
//	?type=      song (default) | mix (DJ mixes, nonstops, mixtapes) | all
//	?sort=      relevance (default with q) | discover (default without q) | recent | plays | title
//	            discover: a weighted shuffle (popularity, reach, freshness,
//	            region), artists spread out; not with q
//	?region=    ug | ke | tz | rw | ng | gh | za | africa | global (discover: in-region songs come up more)
//	?seed=      discover: any text; the same seed gives the same feed. Without one, page 1
//	            is shuffled afresh on every request: send its meta.seed with the next pages
//	?live=      auto (default: the catalog answers; providers are asked live only when that answer is weak) | true | false
//	?page= ?limit=
func (s *Service) List(c *fiber.Ctx) error {
	p := base.ParsePage(c)
	q := Query{Q: c.Query("q"), Artist: c.Query("artist"), Genre: c.Query("genre"), Provider: c.Query("provider"),
		Sort: c.Query("sort"), Page: p.Page, Limit: p.Limit, Live: LiveMode(c.Query("live", string(LiveAuto)))}
	if q.Sort != "" && !slices.Contains(sorts, q.Sort) {
		return base.API_ERROR(c, "sort must be one of: "+strings.Join(sorts, ", "))
	}
	// Browsing (no text) is a shuffled feed unless an order is asked for:
	// the same list on every visit made the app look frozen. Filters the
	// feed doesn't support (provider, hasAudio) keep the plain list.
	if q.Sort == "" && strings.TrimSpace(q.Q) == "" && q.Provider == "" && c.Query("hasAudio") == "" {
		q.Sort = "discover"
	}
	q.Type = c.Query("type", KindSong)
	if !slices.Contains(types, q.Type) {
		return base.API_ERROR(c, "type must be one of: "+strings.Join(types, ", "))
	}
	if q.Live != LiveAuto && q.Live != LiveOn && q.Live != LiveOff {
		return base.API_ERROR(c, "live must be auto, true or false")
	}
	if q.Provider != "" && !slices.ContainsFunc(s.Providers, func(pi ProviderInfo) bool { return pi.Name == q.Provider }) {
		return base.API_ERROR(c, "unknown provider "+q.Provider+"; see /api/v1/providers")
	}
	if v := c.Query("hasAudio"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return base.API_ERROR(c, "hasAudio must be true or false")
		}
		q.HasAudio = &b
	}
	region := strings.ToLower(c.Query("region"))
	if region != "" && !slices.Contains(Regions, region) {
		return base.API_ERROR(c, "region must be one of: "+strings.Join(Regions, ", "))
	}
	if q.Sort == "discover" {
		if strings.TrimSpace(q.Q) != "" {
			return base.API_ERROR(c, "sort=discover can't be combined with q (use sort=relevance to search)")
		}
		seed := c.Query("seed")
		if seed == "" {
			// page 1: a fresh shuffle every time; later pages without the
			// seed: the hour's feed (send meta.seed to page one shuffle)
			seed = freshSeed()
			if p.Page > 1 {
				seed = time.Now().UTC().Format("2006010215")
			}
		}
		songs, total, err := s.Search.Discover(c.UserContext(), DiscoverQuery{Genre: q.Genre, Artist: q.Artist, Region: region,
			Kind: q.Type, Seed: SeedFor(seed, region, q.Genre, q.Artist, q.Type), Page: p.Page, Limit: p.Limit})
		if err != nil {
			return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to build the feed")
		}
		for i := range songs {
			s.orderSources(&songs[i])
		}
		return c.JSON(base.APIResponse{Data: songs, Pagination: p.Controls(total, len(songs)),
			Meta: fiber.Map{"sort": "discover", "seed": seed, "region": region}})
	}
	res, err := s.Search.Search(c.UserContext(), q)
	if err != nil {
		slog.Error("catalog: search failed", "q", q.Q, "page", q.Page, "err", err)
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to search songs")
	}
	for i := range res.Songs {
		s.orderSources(&res.Songs[i])
	}
	meta := fiber.Map{"query": q.Q, "cached": res.Cached}
	if res.Match != "" {
		meta["match"] = res.Match // exact: the first result holds every word; close: typos or some words
	}
	if res.Suggest != "" {
		meta["suggest"] = res.Suggest // did you mean
	}
	if res.Loose {
		meta["loose"] = true // no song had every word: these have some, the most first
	}
	if res.Live != nil {
		meta["live"] = res.Live
		meta["ingested"] = res.Ingested
	}
	return c.JSON(base.APIResponse{Data: res.Songs, Pagination: p.Controls(res.Total, len(res.Songs)), Meta: meta})
}

func (s *Service) orderSources(song *Song) {
	slices.SortStableFunc(song.Sources, func(a, b Source) int {
		return s.Ingester.rank(a.Provider) - s.Ingester.rank(b.Provider)
	})
}

func (s *Service) Get(c *fiber.Ctx) error {
	id, ok := base.ParamID(c, "id")
	if !ok {
		return base.API_ERROR(c, "invalid song id")
	}
	song, err := s.Search.Get(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return base.API_FAIL(c, fiber.StatusNotFound, "song not found")
	} else if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get song")
	}
	s.orderSources(&song)
	return c.JSON(base.APIResponse{Data: song})
}

type providerStatus struct {
	ProviderInfo
	Sources     int64      `json:"sources"`     // provider copies in the catalog
	IndexedUpTo *time.Time `json:"indexedUpTo"` // indexer watermark (index providers)
}

// ProvidersList shows every provider and how much of the catalog it backs.
func (s *Service) ProvidersList(c *fiber.Ctx) error {
	var counts []struct {
		Provider string
		N        int64
	}
	s.DB.Model(&Source{}).Select("provider, COUNT(*) AS n").Group("provider").Scan(&counts)
	byName := map[string]int64{}
	for _, r := range counts {
		byName[r.Provider] = r.N
	}
	var wms []Watermark
	s.DB.Find(&wms)
	wm := map[string]time.Time{}
	for _, w := range wms {
		wm[w.Provider] = w.Since
	}
	out := make([]providerStatus, 0, len(s.Providers))
	for _, p := range s.Providers {
		ps := providerStatus{ProviderInfo: p, Sources: byName[p.Name]}
		if t, ok := wm[p.Name]; ok {
			ps.IndexedUpTo = &t
		}
		out = append(out, ps)
	}
	var songs int64
	s.DB.Model(&Song{}).Count(&songs)
	var multi int64
	s.DB.Model(&Song{}).Where("source_count > 1").Count(&multi)
	return c.JSON(base.APIResponse{Data: out, Meta: fiber.Map{
		"songs": songs, "songsOnSeveralProviders": multi, "priority": s.Ingester.Priority,
		"lastIndexedAt": s.Indexer.LastRun()}})
}

// Reindex pulls everything changed from provider stores now.
func (s *Service) Reindex(c *fiber.Ctx) error {
	out := s.Indexer.RunOnce(c.UserContext())
	s.Search.Bump(c.UserContext())
	return c.JSON(base.APIResponse{Data: out, Message: "indexed"})
}

// Cleanup removes stored songs without an audio URL (data from before the
// audio-only rule; new data without audio is never stored).
func (s *Service) Cleanup(c *fiber.Ctx) error {
	st, err := s.Ingester.PurgeWithoutAudio(time.Now().UTC())
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "cleanup failed")
	}
	s.Search.Bump(c.UserContext())
	return c.JSON(base.APIResponse{Data: st, Message: "removed songs without audio"})
}

// Suggest is search-as-you-type: ?q= (2+ letters), ?limit= (default 8, max 20).
func (s *Service) Suggest(c *fiber.Ctx) error {
	q := c.Query("q")
	if len(q) > 200 {
		return base.API_ERROR(c, "q is too long")
	}
	out, err := s.Search.Complete(c.UserContext(), q, c.QueryInt("limit", 8))
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to suggest songs")
	}
	return c.JSON(base.APIResponse{Data: out})
}

// Next is "up next" after a song, from a taste profile of the song and the
// recent history (see Recommend).
//
//	?limit=    1-50 (default 20)
//	?history=  ids played before it, newest first: shape the taste, never repeated
//	?exclude=  ids never to suggest (skipped, disliked)
//	?region=   favour a region (default: the songs' own)
//	?seed=     any text; the same seed gives the same list (default: changes hourly)
func (s *Service) Next(c *fiber.Ctx) error {
	id, ok := base.ParamID(c, "id")
	if !ok {
		return base.API_ERROR(c, "invalid song id")
	}
	limit := c.QueryInt("limit", 20)
	if limit < 1 || limit > 50 {
		return base.API_ERROR(c, "limit must be 1-50")
	}
	region := strings.ToLower(c.Query("region"))
	if region != "" && !slices.Contains(Regions, region) {
		return base.API_ERROR(c, "region must be one of: "+strings.Join(Regions, ", "))
	}
	exclude, history := idList(c.Query("exclude"), 200), idList(c.Query("history"), 50)
	seed := c.Query("seed")
	if seed == "" {
		seed = freshSeed() // a new list every time unless a seed is sent
	}
	song, songs, err := s.Search.Next(c.UserContext(), id, NextQuery{Region: region, Limit: limit, Exclude: exclude,
		History: history, Seed: SeedFor(seed, strconv.FormatUint(uint64(id), 10), region, c.Query("history"))})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return base.API_FAIL(c, fiber.StatusNotFound, "song not found")
	} else if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to recommend songs")
	}
	for i := range songs {
		s.orderSources(&songs[i])
	}
	return c.JSON(base.APIResponse{Data: songs,
		Meta: fiber.Map{"after": fiber.Map{"id": song.ID, "title": song.Title, "artist": song.Artist}, "seed": seed}})
}

// idList parses "1,2,3" (invalid entries skipped, at most max).
func idList(v string, max int) []uint {
	var out []uint
	for _, x := range strings.Split(v, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(x), 10, 64); err == nil && n > 0 && len(out) < max {
			out = append(out, uint(n))
		}
	}
	return out
}

func nameList(v string, max int) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" && len(out) < max {
			out = append(out, x)
		}
	}
	return out
}

// Recommend: songs fitting a taste built from seeds (Spotify-style radio).
//
//	?songs=       seed song ids, most important first (e.g. recent plays, likes)
//	?artists=     seed artist names, comma-separated
//	?genres=      seed genres, comma-separated
//	(at least one seed; up to 10 songs, 5 artists, 5 genres)
//	?region=      favour a region
//	?minYear= ?maxYear=   release years
//	?popularity=  0-100 target reach (default: the seeds' own; 100 = hits)
//	?exclude=     ids never to suggest
//	?type=        song | mix (default: the first seed song's)
//	?limit= 1-50 (default 20)   ?seed= repeatable shuffle
func (s *Service) Recommend(c *fiber.Ctx) error {
	q := RecommendQuery{Songs: idList(c.Query("songs"), 10), Artists: nameList(c.Query("artists"), 5),
		Genres: nameList(c.Query("genres"), 5), Exclude: idList(c.Query("exclude"), 200),
		MinYear: c.QueryInt("minYear"), MaxYear: c.QueryInt("maxYear"), Limit: c.QueryInt("limit", 20)}
	if len(q.Songs)+len(q.Artists)+len(q.Genres) == 0 {
		return base.API_ERROR(c, "give at least one seed: songs (ids), artists or genres")
	}
	if q.Limit < 1 || q.Limit > 50 {
		return base.API_ERROR(c, "limit must be 1-50")
	}
	q.Region = strings.ToLower(c.Query("region"))
	if q.Region != "" && !slices.Contains(Regions, q.Region) {
		return base.API_ERROR(c, "region must be one of: "+strings.Join(Regions, ", "))
	}
	if v := c.Query("popularity"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 100 {
			return base.API_ERROR(c, "popularity must be 0-100")
		}
		q.Popularity = &f
	}
	if t := c.Query("type"); t != "" {
		if t != KindSong && t != KindMix {
			return base.API_ERROR(c, "type must be song or mix")
		}
		q.Kind = t
	}
	seed := c.Query("seed")
	if seed == "" {
		seed = freshSeed() // a new list every time unless a seed is sent
	}
	q.Seed = SeedFor(seed, c.Query("songs"), c.Query("artists"), c.Query("genres"), q.Region)
	songs, err := s.Search.Recommend(c.UserContext(), q)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return base.API_FAIL(c, fiber.StatusNotFound, "none of the seed songs exist")
	} else if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to recommend songs")
	}
	for i := range songs {
		s.orderSources(&songs[i])
	}
	return c.JSON(base.APIResponse{Data: songs, Meta: fiber.Map{"seed": seed}})
}

// freshSeed: a random seed, returned in meta.seed so a client can replay
// or page the same shuffle.
func freshSeed() string {
	var b [6]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}
