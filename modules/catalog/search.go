package catalog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

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
}

// Searcher service: local catalog search + live fan-out to providers.
type SearchService struct {
	DB        *gorm.DB
	Ingester  *Ingester
	Searchers []Searcher
	Timeout   time.Duration // per provider
	CacheTTL  time.Duration // per (provider, query)

	mu    sync.Mutex
	cache map[string]time.Time // provider|query → when it was last fetched
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
			tracks, err := sr.Search(cctx, q, 50)
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
	if text != "" && (q.Live == LiveOn || (q.Live == LiveAuto && q.Page <= 1)) && len(s.Searchers) > 0 {
		res.Live, res.Ingested = s.live(ctx, text)
	}

	db := s.DB.Model(&Song{})
	folded := Fold(text)
	for _, w := range strings.Fields(folded) {
		like := "%" + likeEscape(w) + "%"
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
	if err := db.Count(&res.Total).Error; err != nil {
		return res, err
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
	limit := min(max(q.Limit, 1), 100)
	page := max(q.Page, 1)
	err := db.Preload("Sources").Offset((page - 1) * limit).Limit(limit).Find(&res.Songs).Error
	if res.Songs == nil {
		res.Songs = []Song{}
	}
	return res, err
}

// Get returns one catalog entry with all its sources.
func (s *SearchService) Get(id uint) (Song, error) {
	var song Song
	err := s.DB.Preload("Sources").First(&song, id).Error
	return song, err
}
