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
	var after uint
	filled := 0
	for ctx.Err() == nil {
		var rows []Song
		if err := s.DB.Select("id", "key", "title", "artist", "featured").
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
		out[w] = ids
		s.Cache.Set(ctx, key, ids, s.CountTTL)
	}
	return out
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
	var wg sync.WaitGroup
	for i, sr := range s.Searchers {
		key := sr.Name() + "|" + Fold(q) // "KENZO  sitya" and "kenzo sitya" share an entry
		if s.cached(key) {
			out[i] = ProviderSearch{Provider: sr.Name(), Status: "cached"}
			continue
		}
		wg.Go(func() {
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
				ps.Status, ps.Error = "skipped", err.Error()
			case cctx.Err() == context.DeadlineExceeded:
				ps.Status, ps.Error = "timeout", "no answer within "+s.Timeout.String()
			case err != nil:
				ps.Status, ps.Error = "error", err.Error()
			default:
				s.remember(key)
				found[i] = tracks
			}
			out[i] = ps
		})
	}
	wg.Wait()
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
	filters := []any{folded, Fold(q.Artist), strings.ToLower(q.Genre), q.Provider, hasAudio}
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
	if text != "" && (q.Live == LiveOn || (q.Live == LiveAuto && q.Page <= 1)) && len(s.Searchers) > 0 {
		res.Live, res.Ingested = s.live(ctx, text)
	}

	words := strings.Fields(folded)
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

	sort := q.Sort
	if sort == "" {
		sort = "recent"
		if text != "" {
			sort = "relevance"
		}
	}
	m := matcher{words: words, indexed: s.textIndexed.Load()}
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
	if res.Ingested.NewSongs > 0 || !s.Cache.Get(ctx, countKey, &res.Total) {
		if err := db.Session(&gorm.Session{}).Count(&res.Total).Error; err != nil {
			return res, err
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
			return res, err
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
