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
	// Budget: the most one search may take, live search included
	// (CATALOG_SEARCH_BUDGET, default 10s).
	Budget time.Duration

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
	// Background runs upkeep (the search_text backfill): no statement
	// timeout, and off the pool requests use. Nil: DB.
	Background *gorm.DB
	// BackfillPause is slept between PrepareSearchText batches so a
	// backfill of the whole catalog never saturates a small database.
	BackfillPause time.Duration

	mu    sync.Mutex
	cache map[string]time.Time // provider|query → when it was last fetched
}

const cacheNS = "catalog"

// FilteredCountCap: a filtered search counts up to this many matches, no further
const FilteredCountCap = 5000

// Redis keys remembering upkeep already done on the existing data, so a
// restart neither falls back to the slow per-column search nor rescans
// the whole catalog.
const (
	readyKey       = "catalog:searchtext:ready"
	backfilledKey  = "catalog:searchtext:done:v"
	upkeepStateTTL = 30 * 24 * time.Hour
)

func (s *SearchService) bg() *gorm.DB {
	if s.Background != nil {
		return s.Background
	}
	return s.DB
}

// RestoreIndexState switches text queries to the search_text index at
// once when a previous run already filled it, so a restart never falls
// back to the per-column scan (which made every search time out until the
// upkeep got to it, minutes later). Redis remembers it, but Redis lives in
// the container and a deploy empties it: Postgres is asked too. The trigram
// index on search_text is only built once the backfill is done, so its
// existence (and validity) is proof enough. Both checks are instant.
func (s *SearchService) RestoreIndexState(ctx context.Context) {
	if s.Cache.Exists(ctx, readyKey) {
		s.textIndexed.Store(true)
		return
	}
	if s.DB.Dialector.Name() != "postgres" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ready bool
	err := s.bg().WithContext(cctx).Raw(`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = 'idx_catalog_songs_search_trgm' AND i.indisvalid)`).Scan(&ready).Error
	if err == nil && ready {
		s.markIndexed(ctx)
		slog.Info("catalog: search index ready (found in the database)")
	}
}

func (s *SearchService) markIndexed(ctx context.Context) {
	s.textIndexed.Store(true)
	s.Cache.Set(ctx, readyKey, true, upkeepStateTTL)
}

// Bump drops every cached result and total (after bulk changes).
func (s *SearchService) Bump(ctx context.Context) { s.Cache.Bump(ctx, cacheNS) }

// PrepareSearchText (re)builds search_text for rows stored before the
// column existed or with an older SearchTextVersion, in batches. Text
// queries use the column as soon as no row is left empty (an older version
// still matches, just without the newer forms). Cheap when there is nothing
// to do; safe to run while the API serves.
func (s *SearchService) PrepareSearchText(ctx context.Context) error {
	const batch = 1000
	db := s.bg()
	// A few rows without search_text (old ones being filled) don't make
	// search worse; only a catalog mostly without it needs the slow path.
	// With idx_catalog_songs_search_empty this reads a tiny index.
	var empties int64
	if err := db.Raw(`SELECT COUNT(*) FROM (SELECT 1 FROM catalog_songs WHERE search_text = '' LIMIT 500) AS e`).Scan(&empties).Error; err != nil {
		return err
	}
	if empties < 500 {
		s.markIndexed(ctx)
	}
	// Existing data already rebuilt at this version (by a previous run):
	// only rows still empty need work, found from the partial index; no
	// pass over the whole catalog on every start.
	doneKey := backfilledKey + fmt.Sprint(SearchTextVersion)
	versionDone := s.Cache.Exists(ctx, doneKey)
	if !versionDone {
		// MixMinMs shrank (25 -> 20 min): one targeted update instead of a
		// full rewrite. Cheap when nothing matches.
		if err := db.Model(&Song{}).Where("kind = ? AND duration_ms >= ?", KindSong, MixMinMs).Update("kind", KindMix).Error; err != nil {
			return err
		}
	}
	var after uint
	filled := 0
	for ctx.Err() == nil {
		var rows []Song
		q := db.Select("id", "key", "title", "artist", "featured", "duration_ms").Where("id > ?", after)
		if versionDone {
			q = q.Where("search_text = ''")
		} else {
			q = q.Where("search_version < ? OR search_text = ''", SearchTextVersion)
		}
		if err := q.Order("id").Limit(batch).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			s.markIndexed(ctx)
			s.Cache.Set(ctx, doneKey, true, upkeepStateTTL)
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
		if err := db.Exec(sql.String(), append(args, SearchTextVersion, ids)...).Error; err != nil {
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
			if err := db.Model(&Song{}).Where("id IN ?", mixes).Update("kind", KindMix).Error; err != nil {
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
		// Each lookup has its own deadline: on a busy database, typo
		// matching is dropped for this word rather than failing the search
		// (and a failure is never cached as "no typo matches").
		wctx, cancel := context.WithTimeout(ctx, fuzzyWordTimeout)
		var n int64
		if err := s.DB.WithContext(wctx).Raw(`SELECT COUNT(*) FROM (SELECT 1 FROM catalog_songs WHERE search_text LIKE ? ESCAPE '\' LIMIT ?) hits`,
			"%"+likeEscape(w)+"%", rareWordHits).Scan(&n).Error; err != nil {
			cancel()
			continue
		}
		ids = []uint{}
		if n < rareWordHits {
			// the closest few hundred only: a typo stands for one intended
			// word, not every word sharing some letters with it
			if err := s.DB.WithContext(wctx).Raw(`SELECT id FROM catalog_songs WHERE ? <% search_text ORDER BY word_similarity(?, search_text) DESC, id LIMIT ?`,
				w, w, fuzzyMaxSongs).Scan(&ids).Error; err != nil {
				cancel()
				continue
			}
		}
		if n == 0 && len(ids) == 0 {
			// Nothing as typed, nothing within the usual typo distance: look
			// further rather than answer nothing ("essense" -> "Essence",
			// similarity 0.57). Few songs only: these are long shots.
			loose, ok := s.looseIDs(wctx, w)
			if !ok {
				cancel()
				continue
			}
			ids = loose
		}
		cancel()
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
func (s *SearchService) looseIDs(ctx context.Context, w string) ([]uint, bool) {
	var ids []uint
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(fmt.Sprintf("SET LOCAL pg_trgm.word_similarity_threshold = %.2f", looseFuzzyThreshold)).Error; err != nil {
			return err
		}
		return tx.Raw(`SELECT id FROM catalog_songs WHERE ? <% search_text ORDER BY word_similarity(?, search_text) DESC, id LIMIT ?`,
			w, w, fuzzyMaxSongs/4).Scan(&ids).Error
	})
	if err != nil {
		return nil, false
	}
	if ids == nil {
		ids = []uint{}
	}
	return ids, true
}

// fuzzyWordTimeout: the most typo matching may take per word.
const fuzzyWordTimeout = 2500 * time.Millisecond

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
func (s *SearchService) live(ctx context.Context, q string, timeout time.Duration) ([]ProviderSearch, IngestStats) {
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
		out[i] = ProviderSearch{Provider: sr.Name(), Status: "timeout", Error: "no answer within " + timeout.String()}
		go func() {
			start := time.Now()
			cctx, cancel := context.WithTimeout(ctx, timeout)
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
				ps.Status, ps.Error, tracks = "timeout", "no answer within "+timeout.String(), nil
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
	deadline := time.NewTimer(timeout + liveSlack(timeout))
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
	// One budget for the whole request, live search included: a gateway in
	// front (often ~15s) must never time out first.
	budget := s.Budget
	if budget <= 0 {
		budget = 10 * time.Second
	}
	deadline := time.Now().Add(budget)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
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
		res.Live, res.Ingested = s.live(ctx, text, s.liveTimeout(budget))
	}

	// local runs the catalog query; it may run twice (after a live search).
	local := func() error {
		res.Songs, res.Match, res.Loose, res.Suggest = nil, "", false, ""
		words := strings.Fields(SearchFold(text))
		// filters: everything but the text
		db := s.DB.WithContext(ctx).Model(&Song{})
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
		candidates := int64(-1) // relevance: how many candidates were scored
		m := matcher{words: words, folded: folded, indexed: s.textIndexed.Load()}
		if s.Fuzzy && m.indexed {
			m.fuzzy = s.fuzzyIDs(ctx, gen, words)
		}
		if len(words) > 0 {
			if sort == "relevance" {
				// Blend: any song matching a word (as typed, squashed, or with a
				// typo) is a candidate; the score below ranks the closest first.
				// Only a bounded candidate set is scored (see candidates).
				ids, err := s.candidateIDs(ctx, m)
				if err != nil {
					return err
				}
				candidates = int64(len(ids))
				if len(ids) == 0 {
					db = db.Where("1 = 0")
				} else {
					db = db.Where("id IN ?", ids)
				}
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
		// A count the database can't answer in time (it is strained) falls
		// back to the last one known: a total a few minutes old beats an error.
		if res.Ingested.NewSongs > 0 || !s.Cache.Get(ctx, countKey, &res.Total) {
			lastKey := "count-last:" + cacheKey(filters...)
			filtered := len(words) > 0 || q.Genre != "" || q.Artist != "" || q.Provider != ""
			var n int64
			var err error
			if filtered {
				// a short deadline: a total is worth less than the answer
				cctx, cancel := context.WithTimeout(ctx, countTimeout)
				sub := db.Session(&gorm.Session{}).WithContext(cctx).Select("1").Limit(FilteredCountCap)
				err = s.DB.WithContext(cctx).Table("(?) AS capped", sub).Count(&n).Error
				cancel()
			} else if s.Cache.Get(ctx, lastKey, &n) {
				// the whole catalog: RefreshTotals keeps this current in the
				// background; counting 600k rows per request starved the database
				err = nil
			} else {
				err = db.Session(&gorm.Session{}).Count(&n).Error
			}
			switch {
			case err == nil:
				res.Total = n
				s.Cache.Set(ctx, countKey, n, s.CountTTL)
				s.Cache.Set(ctx, lastKey, n, upkeepStateTTL)
			case s.Cache.Get(ctx, lastKey, &res.Total):
			case candidates >= 0:
				res.Total = candidates // at most this many match: the page still answers
			case !filtered && s.estimate(ctx, &res.Total):
				// no total known yet: Postgres's own row estimate
			default:
				return err
			}
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
	// providers live, then answers again with what they added. All of it
	// within the request's budget: the live search gets what's left (minus
	// time for the second answer), or is skipped; a second answer that
	// fails keeps the first.
	if canLive && q.Live == LiveAuto && page == 1 && (res.Match != "exact" || res.Total < int64(limit)) {
		// skip it only when too little time is left to be worth asking
		if lt := s.liveTimeout(time.Until(deadline)); lt > 0 && lt >= min(500*time.Millisecond, s.liveTimeout(budget)) {
			first := res
			res.Live, res.Ingested = s.live(ctx, text, lt)
			if res.Ingested.NewSongs > 0 || res.Ingested.Attached > 0 || res.Ingested.Inserted > 0 {
				if err := local(); err != nil {
					live, ing := res.Live, res.Ingested
					res = first
					res.Live, res.Ingested = live, ing
				}
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

// estimate: the planner's row count for catalog_songs (Postgres; instant).
func (s *SearchService) estimate(ctx context.Context, n *int64) bool {
	if s.DB.Dialector.Name() != "postgres" {
		return false
	}
	var est float64
	if err := s.DB.WithContext(ctx).Raw(`SELECT reltuples FROM pg_class WHERE relname = 'catalog_songs'`).Scan(&est).Error; err != nil || est <= 0 {
		return false
	}
	*n = int64(est)
	return true
}

// RefreshTotals counts the unfiltered catalog (per type, with and without
// the audio filter) on the background pool and stores the totals the
// browse endpoints read, so no request counts the whole table.
func (s *SearchService) RefreshTotals(ctx context.Context) error {
	db := s.bg().WithContext(ctx)
	for _, kind := range []string{KindSong, KindMix, "all"} {
		for _, hasAudio := range []string{"", "true"} {
			q := db.Model(&Song{})
			if kind != "all" {
				q = q.Where("kind = ?", kind)
			}
			if hasAudio == "true" {
				q = q.Where("audio_url <> ''")
			}
			var n int64
			if err := q.Count(&n).Error; err != nil {
				return err
			}
			filters := []any{"", "", "", "", hasAudio, kind}
			s.Cache.Set(ctx, "count-last:"+cacheKey(filters...), n, upkeepStateTTL)
		}
	}
	return nil
}

// liveSlack: how long past the providers' timeout their answers are awaited.
func liveSlack(timeout time.Duration) time.Duration { return min(2*time.Second, timeout/2) }

// liveTimeout: the provider timeout that fits in left, keeping a quarter
// of it for the catalog's second answer (CATALOG_LIVE_TIMEOUT at most).
func (s *SearchService) liveTimeout(left time.Duration) time.Duration {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	avail := left * 3 / 4
	// t + liveSlack(t) <= avail
	t := max(avail-2*time.Second, avail*2/3)
	return max(0, min(timeout, t))
}

// candidatePartTimeout: the most one candidate lookup may take.
const candidatePartTimeout = 4 * time.Second

// candidateIDs runs the candidate lookups in parallel, each with its own
// deadline, and merges their ids (typo matches included). A lookup that
// fails or is too slow is dropped; only when every one fails (and there
// are no typo matches) is that an error.
func (s *SearchService) candidateIDs(ctx context.Context, m matcher) ([]uint, error) {
	qs := m.candidateQueries()
	results := make([][]uint, len(qs))
	errs := make([]error, len(qs))
	var wg sync.WaitGroup
	for i, q := range qs {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, candidatePartTimeout)
			defer cancel()
			errs[i] = s.DB.WithContext(cctx).Raw(q.SQL, q.Vars...).Scan(&results[i]).Error
		})
	}
	wg.Wait()
	seen := map[uint]bool{}
	var ids []uint
	add := func(list []uint) {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	var firstErr error
	ok := 0
	for i := range qs {
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = errs[i]
			}
			slog.Warn("catalog: candidate lookup dropped", "part", i, "err", errs[i])
			continue
		}
		ok++
		add(results[i])
	}
	add(m.fuzzyCandidates())
	if ok == 0 && len(ids) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return ids, nil
}

// countTimeout: the most a filtered total may take.
const countTimeout = 3 * time.Second
