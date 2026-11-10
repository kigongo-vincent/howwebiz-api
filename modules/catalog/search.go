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
}

// Searcher service: local catalog search + live fan-out to providers.
type SearchService struct {
	DB        *gorm.DB
	Ingester  *Ingester
	Searchers []Searcher
	Timeout   time.Duration // per provider
	CacheTTL  time.Duration // per (provider, query)

	// Redis (optional): whole results for ResultTTL, totals for CountTTL.
	// Totals are the expensive part of a broad query and change slowly, so
	// they outlive the pages; Bump (reindex, cleanup) drops both at once.
	Cache     *cache.Cache
	ResultTTL time.Duration
	CountTTL  time.Duration

	// textIndexed: every row has search_text, so text queries use its
	// trigram index (until then, the original per-column match).
	textIndexed atomic.Bool

	mu    sync.Mutex
	cache map[string]time.Time // provider|query → when it was last fetched
}

const cacheNS = "catalog"

// Bump drops every cached result and total (after bulk changes).
func (s *SearchService) Bump(ctx context.Context) { s.Cache.Bump(ctx, cacheNS) }

// PrepareSearchText fills search_text for rows stored before the column
// existed, in batches, then switches text queries to its index. Cheap when
// there is nothing to do; safe to run while the API serves.
func (s *SearchService) PrepareSearchText(ctx context.Context) error {
	const batch = 1000
	var after uint
	filled := 0
	for ctx.Err() == nil {
		var rows []Song
		if err := s.DB.Select("id", "key", "title", "artist", "featured").
			Where("search_text = '' AND id > ?", after).Order("id").Limit(batch).Find(&rows).Error; err != nil {
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
		sql.WriteString(" END WHERE id IN ?")
		if err := s.DB.Exec(sql.String(), append(args, ids)...).Error; err != nil {
			return err
		}
		filled += len(rows)
		after = rows[len(rows)-1].ID
	}
	return ctx.Err()
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

	db := s.DB.Model(&Song{})
	for _, w := range strings.Fields(folded) {
		like := "%" + likeEscape(w) + "%"
		if s.textIndexed.Load() {
			// one trigram-indexed column holding every searchable field, folded
			db = db.Where(`search_text LIKE ? ESCAPE '\'`, like)
			continue
		}
		// Match on the folded matching key plus display fields, so
		// accents/punctuation never prevent a hit.
		db = db.Where(`(key LIKE ? ESCAPE '\' OR LOWER(title) LIKE ? ESCAPE '\' OR LOWER(artist) LIKE ? ESCAPE '\' OR LOWER(CAST(featured AS TEXT)) LIKE ? ESCAPE '\')`,
			like, like, like, like)
	}
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
	// Live results just ingested may change the total: count afresh then.
	if res.Ingested.NewSongs > 0 || !s.Cache.Get(ctx, countKey, &res.Total) {
		if err := db.Session(&gorm.Session{}).Count(&res.Total).Error; err != nil {
			return res, err
		}
		s.Cache.Set(ctx, countKey, res.Total, s.CountTTL)
	}

	sort := q.Sort
	if sort == "" {
		sort = "recent"
		if text != "" {
			sort = "relevance"
		}
	}
	switch sort {
	case "relevance":
		// exact title, title prefix, artist match, then popularity.
		// One clause.OrderBy expression holding the whole ordering: GORM drops
		// a bare gorm.Expr in Order(), and chaining another Order() after an
		// expression discards the expression (OrderBy.MergeClause keeps only
		// columns).
		db = db.Order(clause.OrderBy{Expression: clause.Expr{
			SQL: `CASE WHEN key LIKE ? ESCAPE '\' THEN 0 WHEN key LIKE ? ESCAPE '\' THEN 1 WHEN key LIKE ? ESCAPE '\' THEN 2 ELSE 3 END, ` +
				`source_count DESC, plays DESC, id DESC`,
			Vars:               []any{"%|" + likeEscape(folded), "%|" + likeEscape(folded) + "%", likeEscape(folded) + "|%"},
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
	s.Cache.Set(ctx, resultKey, res, s.ResultTTL)
	return res, nil
}

// Get returns one catalog entry with all its sources.
func (s *SearchService) Get(id uint) (Song, error) {
	var song Song
	err := s.DB.Preload("Sources").First(&song, id).Error
	return song, err
}
