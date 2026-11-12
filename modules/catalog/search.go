package catalog

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/cache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Query is an aggregated catalog query.
type Query struct {
	Q        string
	Artist   string
	Genre    string
	Provider string // only songs available from this provider
	HasAudio *bool
	Type     string // song (default) | mix | all
	Sort     string // relevance (default with q) | recent (default) | plays | title
	Page     int
	Limit    int
	Live     LiveMode
}

type LiveMode string

const (
	LiveAuto LiveMode = "auto" // first page of a text search: ask providers too
	LiveOn   LiveMode = "true"
	LiveOff  LiveMode = "false"
)

// ProviderSearch reports what a live provider search did.
type ProviderSearch struct {
	Provider string `json:"provider"`
	Status   string `json:"status"` // ok | cached | skipped | error | timeout
	Results  int    `json:"results"`
	TookMs   int64  `json:"tookMs"`
	Error    string `json:"error,omitempty"`
}

type Result struct {
	Songs []Song
	Total int64
	Live  []ProviderSearch
	// Ingested is what the live searches added to the catalog.
	Ingested IngestStats
	Cached   bool // served from Redis
	// Match is how close the best result is: "exact" (every word, as typed
	// or squashed) or "close" (typos or only some words); empty without q.
	Match string
	// Loose: the best result is only a close match (Match == "close")
	Loose bool
	// Suggest is the query respelled after the best result ("did you mean
	// essence wizkid"), when that result was only a close match.
	Suggest string
}

// Searcher service: local catalog search + live fan-out to providers.
type SearchService struct {
	DB        *gorm.DB
	Ingester  *Ingester
	Searchers []Searcher
	Timeout   time.Duration // per provider
	CacheTTL  time.Duration // per (provider, query)

	// Fuzzy enables typo-tolerant matching (Postgres with pg_trgm).
	Fuzzy bool

	// Redis (optional): whole results for ResultTTL, totals for CountTTL.
	// Totals are the expensive part of a broad query and change slowly, so
	// they outlive the pages; Bump (reindex, cleanup) drops both at once.
	Cache     *cache.Cache
	ResultTTL time.Duration
	CountTTL  time.Duration

	// textIndexed: every row has search_text, so text queries use its
	// trigram index (until then, the original per-column match).
	textIndexed atomic.Bool
	// BackfillPause is slept between PrepareSearchText batches so a
	// backfill of the whole catalog never saturates a small database.
	BackfillPause time.Duration

	mu    sync.Mutex
	cache map[string]time.Time // provider|query → when it was last fetched
}

const cacheNS = "catalog"

// FilteredCountCap: a filtered search counts up to this many matches, no further
const FilteredCountCap = 5000

// Bump drops every cached result and total (after bulk changes).
func (s *SearchService) Bump(ctx context.Context) { s.Cache.Bump(ctx, cacheNS) }

// PrepareSearchText (re)builds search_text for rows stored before the
// column existed or with an older SearchTextVersion, in batches. Text
// queries use the column as soon as no row is left empty (an older version
// still matches, just without the newer forms). Cheap when there is nothing
// to do; safe to run while the API serves.
func (s *SearchService) PrepareSearchText(ctx context.Context) error {
	const batch = 1000
	var empty bool
	if err := s.DB.Raw(`SELECT EXISTS (SELECT 1 FROM catalog_songs WHERE search_text = '')`).Scan(&empty).Error; err != nil {
		return err
	}
	if !empty {
		s.textIndexed.Store(true)
	}
	// MixMinMs shrank (25 -> 20 min): one targeted update instead of a
	// full rewrite. Cheap when nothing matches.
	if err := s.DB.Model(&Song{}).Where("kind = ? AND duration_ms >= ?", KindSong, MixMinMs).Update("kind", KindMix).Error; err != nil {
		return err
	}
	var after uint
	filled := 0
	for ctx.Err() == nil {
		var rows []Song
		if err := s.DB.Select("id", "key", "title", "artist", "featured", "duration_ms").
			Where("(search_version < ? OR search_text = '') AND id > ?", SearchTextVersion, after).Order("id").Limit(batch).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			s.textIndexed.Store(true)
			if filled > 0 {
				slog.Info("catalog: search index ready", "backfilled", filled)
			}
			return nil
		}
		var sql strings.Builder
		args := make([]any, 0, 2*len(rows)+1)
		ids := make([]uint, len(rows))
		sql.WriteString("UPDATE catalog_songs SET search_text = CASE id")
		for i, r := range rows {
			sql.WriteString(" WHEN ? THEN ?")
			args = append(args, r.ID, SearchText(r.Title, r.Artist, r.Featured, r.Key))
			ids[i] = r.ID
		}
		sql.WriteString(" END, search_version = ? WHERE id IN ?")
		if err := s.DB.Exec(sql.String(), append(args, SearchTextVersion, ids)...).Error; err != nil {
			return err
		}
		// mixes stored before kind existed
		var mixes []uint
		for _, r := range rows {
			if IsMix(r.Title, r.DurationMs) {
				mixes = append(mixes, r.ID)
			}
		}
		if len(mixes) > 0 {
			if err := s.DB.Model(&Song{}).Where("id IN ?", mixes).Update("kind", KindMix).Error; err != nil {
				return err
			}
		}
		filled += len(rows)
		after = rows[len(rows)-1].ID
		if filled%(20*batch) == 0 {
			slog.Info("catalog: search index backfill", "filled", filled)
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.BackfillPause):
		}
	}
	return ctx.Err()
}

// rareWordHits: a word held by at least this many songs as typed is
// spelled right; typo matching for it would only add noise.
const rareWordHits = 50

// fuzzyIDs returns, for each query word worth matching with typos, the
// songs closest to it: at most fuzzyMaxSongs, by pg_trgm word_similarity
// over the threshold (the <% operator, answered by the search_text GIN
// index). A word qualifies when it is long enough and found as typed in
// fewer than rareWordHits songs. Each word is computed once and cached:
// it is the costliest step of a search (~0.5s when the typo is close to a
// very common word).
func (s *SearchService) fuzzyIDs(ctx context.Context, gen string, words []string) map[string][]uint {
	out := map[string][]uint{}
	for _, w := range words {
		if utf8.RuneCountInString(w) < fuzzyMinRunes {
			continue
		}
		if _, done := out[w]; done {
			continue
		}
		key := "fuzzy:" + gen + ":" + cacheKey(w)
		var ids []uint
		if s.Cache.Get(ctx, key, &ids) {
			out[w] = ids
			continue
		}
		var n int64
		if err := s.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM (SELECT 1 FROM catalog_songs WHERE search_text LIKE ? ESCAPE '\' LIMIT ?) hits`,
			"%"+likeEscape(w)+"%", rareWordHits).Scan(&n).Error; err != nil {
			continue
		}
		ids = []uint{}
		if n < rareWordHits {
			// the closest few hundred only: a typo stands for one intended
			// word, not every word sharing some letters with it
			if err := s.DB.WithContext(ctx).Raw(`SELECT id FROM catalog_songs WHERE ? <% search_text ORDER BY word_similarity(?, search_text) DESC, id LIMIT ?`,
				w, w, fuzzyMaxSongs).Scan(&ids).Error; err != nil {
				continue
			}
		}
		if n == 0 && len(ids) == 0 {
			// Nothing as typed, nothing within the usual typo distance: look
			// further rather than answer nothing ("essense" -> "Essence",
			// similarity 0.57). Few songs only: these are long shots.
			ids = s.looseIDs(ctx, w)
		}
		out[w] = ids
		s.Cache.Set(ctx, key, ids, s.CountTTL)
	}
	return out
}

// looseFuzzyThreshold: the word similarity a last-resort typo match needs
// (pg_trgm's usual threshold is 0.6).
const looseFuzzyThreshold = 0.45

// looseIDs finds the songs closest to w at looseFuzzyThreshold: the GIN
// index answers <% at whatever threshold the transaction sets.
func (s *SearchService) looseIDs(ctx context.Context, w string) []uint {
	var ids []uint
	_ = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(fmt.Sprintf("SET LOCAL pg_trgm.word_similarity_threshold = %.2f", looseFuzzyThreshold)).Error; err != nil {
			return err
		}
		return tx.Raw(`SELECT id FROM catalog_songs WHERE ? <% search_text ORDER BY word_similarity(?, search_text) DESC, id LIMIT ?`,
			w, w, fuzzyMaxSongs/4).Scan(&ids).Error
	})
	if ids == nil {
		ids = []uint{}
	}
	return ids
}

// cacheKey is a stable digest of the parts of a query that shape a result.
func cacheKey(parts ...any) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%q", parts)))
	return hex.EncodeToString(h[:12])
}

func (s *SearchService) cached(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]time.Time{}
	}
	t, ok := s.cache[key]
	return ok && time.Since(t) < s.CacheTTL
}

func (s *SearchService) remember(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) > 10000 {
		s.cache = map[string]time.Time{}
	}
	s.cache[key] = time.Now()
}

// live asks every provider in parallel; results are ingested so the
// local query that follows sees them merged with everything else.
func (s *SearchService) live(ctx context.Context, q string) ([]ProviderSearch, IngestStats) {
	q = strings.TrimSpace(q)
	out := make([]ProviderSearch, len(s.Searchers))
	found := make([][]Track, len(s.Searchers))
	type answer struct {
		i      int
		ps     ProviderSearch
		tracks []Track
	}
	answers := make(chan answer, len(s.Searchers)) // buffered: late answers never block
	asked := 0
	for i, sr := range s.Searchers {
		key := sr.Name() + "|" + Fold(q) // "KENZO  sitya" and "kenzo sitya" share an entry
		if s.cached(key) {
			out[i] = ProviderSearch{Provider: sr.Name(), Status: "cached"}
			continue
		}
		asked++
		out[i] = ProviderSearch{Provider: sr.Name(), Status: "timeout", Error: "no answer within " + s.Timeout.String()}
		go func() {
			start := time.Now()
			cctx, cancel := context.WithTimeout(ctx, s.Timeout)
			defer cancel()
			var tracks []Track
			var err error
			if perr := base.Safe("live search: "+sr.Name(), func() { tracks, err = sr.Search(cctx, q, 50) }); perr != nil {
				tracks, err = nil, perr
			}
			ps := ProviderSearch{Provider: sr.Name(), Status: "ok", Results: len(tracks), TookMs: time.Since(start).Milliseconds()}
			switch {
			case errors.Is(err, ErrUnavailable):
				ps.Status, ps.Error, tracks = "skipped", err.Error(), nil
			case cctx.Err() == context.DeadlineExceeded:
				ps.Status, ps.Error, tracks = "timeout", "no answer within "+s.Timeout.String(), nil
			case err != nil:
				ps.Status, ps.Error, tracks = "error", err.Error(), nil
			default:
				s.remember(key)
			}
			answers <- answer{i, ps, tracks}
		}()
	}
	// Wait for the answers, but never much past the providers' timeout: a
	// provider stuck outside its context (e.g. waiting on a busy database)
	// must not hold the request. Its answer, if it comes, is dropped.
	deadline := time.NewTimer(s.Timeout + 2*time.Second)
	defer deadline.Stop()
collect:
	for range asked {
		select {
		case a := <-answers:
			out[a.i], found[a.i] = a.ps, a.tracks
		case <-deadline.C:
			break collect
		case <-ctx.Done():
			break collect
		}
	}
	var all []Track
	for _, f := range found {
		all = append(all, f...)
	}
	st, _ := s.Ingester.Ingest(all, time.Now().UTC())
	return out, st
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Search runs the query against the catalog, after a live provider search
// when the mode asks for one.
func (s *SearchService) Search(ctx context.Context, q Query) (Result, error) {
	var res Result
	text := strings.TrimSpace(q.Q)
	folded := Fold(text)
	limit := min(max(q.Limit, 1), 100)
	page := max(q.Page, 1)

	// Normalized filters: two spellings of one query share cache entries.
	var hasAudio string
	if q.HasAudio != nil {
		hasAudio = fmt.Sprint(*q.HasAudio)
	}
	kind := q.Type
	if kind == "" {
		kind = KindSong
	}
	filters := []any{folded, Fold(q.Artist), strings.ToLower(q.Genre), q.Provider, hasAudio, kind}
	gen := s.Cache.Generation(ctx, cacheNS)
	resultKey := "songs:" + gen + ":" + cacheKey(append(filters, q.Sort, page, limit)...)
	countKey := "count:" + gen + ":" + cacheKey(filters...)

	// A cached page answers at once, live provider search included: it
	// already ran (and was ingested) when the page was cached. live=true
	// always asks the providers again.
	if q.Live != LiveOn && s.Cache.Get(ctx, resultKey, &res) {
		res.Cached = true
		return res, nil
	}
	canLive := text != "" && len(s.Searchers) > 0
	// live=true: ask the providers first, always
	if canLive && q.Live == LiveOn {
		res.Live, res.Ingested = s.live(ctx, text)
	}

	// local runs the catalog query; it may run twice (after a live search).
	local := func() error {
		res.Songs, res.Match, res.Loose, res.Suggest = nil, "", false, ""
		words := strings.Fields(SearchFold(text))
		// filters: everything but the text
		db := s.DB.Model(&Song{})
		if q.Artist != "" {
			db = db.Where("key LIKE ? ESCAPE '\\'", likeEscape(Fold(q.Artist))+"|%")
		}
		if q.Genre != "" {
			db = db.Where("LOWER(genre) = ?", strings.ToLower(q.Genre))
		}
		if q.Provider != "" {
			db = db.Where("id IN (?)", s.DB.Model(&Source{}).Select("song_id").Where("provider = ?", q.Provider))
		}
		if q.HasAudio != nil {
			if *q.HasAudio {
				db = db.Where("audio_url <> ''")
			} else {
				db = db.Where("audio_url = ''")
			}
		}
		if kind != "all" {
			db = db.Where("kind = ?", kind)
		}

		sort := q.Sort
		if sort == "" {
			sort = "recent"
			if text != "" {
				sort = "relevance"
			}
		}
		m := matcher{words: words, folded: folded, indexed: s.textIndexed.Load()}
		if s.Fuzzy && m.indexed {
			m.fuzzy = s.fuzzyIDs(ctx, gen, words)
		}
		if len(words) > 0 {
			if sort == "relevance" {
				// Blend: any song matching a word (as typed, squashed, or with a
				// typo) is a candidate; the score below ranks the closest first.
				db = db.Where(m.any())
			} else {
				// An explicit order (recent, plays, title) lists only songs
				// matching every word, so partial matches never lead the list.
				db = db.Where(m.all())
			}
		}
		// Live results just ingested may change the total: count afresh then.
		// A filtered list (text, genre, artist, provider) counts at most
		// FilteredCountCap matches: an exact count of "pop" or "worship"
		// walked tens of thousands of rows per request and, with many at
		// once, starved the database for minutes. Nobody pages that far; the
		// total reads "N+" past the cap (TotalCapped says so).
		if res.Ingested.NewSongs > 0 || !s.Cache.Get(ctx, countKey, &res.Total) {
			filtered := len(words) > 0 || q.Genre != "" || q.Artist != "" || q.Provider != ""
			if filtered {
				sub := db.Session(&gorm.Session{}).Select("1").Limit(FilteredCountCap)
				if err := s.DB.Table("(?) AS capped", sub).Count(&res.Total).Error; err != nil {
					return err
				}
			} else if err := db.Session(&gorm.Session{}).Count(&res.Total).Error; err != nil {
				return err
			}
			s.Cache.Set(ctx, countKey, res.Total, s.CountTTL)
		}

		switch sort {
		case "relevance":
			// closest match first, then: exact title, title prefix, artist
			// match, then popularity. One clause.OrderBy expression holding the
			// whole ordering: GORM drops a bare gorm.Expr in Order(), and chaining
			// another Order() after an expression discards the expression.
			score := m.score()
			db = db.Order(clause.OrderBy{Expression: clause.Expr{
				SQL: score.SQL + ` DESC, CASE WHEN key LIKE ? ESCAPE '\' THEN 0 WHEN key LIKE ? ESCAPE '\' THEN 1 WHEN key LIKE ? ESCAPE '\' THEN 2 ELSE 3 END, ` +
					`source_count DESC, plays DESC, id DESC`,
				Vars:               append(score.Vars, "%|"+likeEscape(folded), "%|"+likeEscape(folded)+"%", likeEscape(folded)+"|%"),
				WithoutParentheses: true}})
		case "plays":
			db = db.Order("plays DESC").Order("id DESC")
		case "title":
			db = db.Order("LOWER(title)").Order("id")
		default: // recent
			db = db.Order("first_seen_at DESC").Order("id DESC")
		}
		// Nothing past the last match: skip the query (deep pages are the
		// costliest OFFSETs).
		if int64((page-1)*limit) < res.Total {
			if err := db.Preload("Sources").Offset((page - 1) * limit).Limit(limit).Find(&res.Songs).Error; err != nil {
				return err
			}
		}
		if res.Songs == nil {
			res.Songs = []Song{}
		}
		if len(words) > 0 && len(res.Songs) > 0 {
			// exact: the best result holds every word as typed (or squashed);
			// close: it doesn't, so these are typo or partial matches
			res.Match = "close"
			top := res.Songs[0]
			if containsAll(SearchText(top.Title, top.Artist, top.Featured, top.Key), words) {
				res.Match = "exact"
			}
			res.Loose = res.Match == "close"
			if res.Loose {
				res.Suggest = Suggest(folded, top)
			}
		}
		return nil
	}
	if err := local(); err != nil {
		return res, err
	}
	// live=auto (default): the catalog answers first, in milliseconds. Only
	// a weak answer (nothing exact, or less than a page) on page 1 asks the
	// providers live (up to CATALOG_LIVE_TIMEOUT), then answers again with
	// what they added.
	if canLive && q.Live == LiveAuto && page == 1 && (res.Match != "exact" || res.Total < int64(limit)) {
		res.Live, res.Ingested = s.live(ctx, text)
		if res.Ingested.NewSongs > 0 || res.Ingested.Attached > 0 || res.Ingested.Inserted > 0 {
			if err := local(); err != nil {
				return res, err
			}
		}
	}
	s.Cache.Set(ctx, resultKey, res, s.ResultTTL)
	return res, nil
}

// Get returns one catalog entry with all its sources.
func (s *SearchService) Get(id uint) (Song, error) {
	var song Song
	err := s.DB.Preload("Sources").First(&song, id).Error
	return song, err
}
