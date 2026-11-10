package search

import (
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SearchService struct {
	DB      *gorm.DB
	Howwe   *HowweSearch
	BaseURL string
	trigram bool // pg_trgm available → fuzzy fallback
}

func NewService(db *gorm.DB) *SearchService {
	return &SearchService{
		DB:      db,
		Howwe:   HowweFromEnv(),
		BaseURL: strings.TrimRight(base.Env("CRAWL_BASE_URL", "https://www.howwe.ug"), "/"),
		trigram: hasTrigram(db),
	}
}

// Setup enables pg_trgm on Postgres (fast; the indexes that use it are
// built in the background, see Indexes). Without it fuzzy search is off.
func Setup(db *gorm.DB) {
	if db.Dialector.Name() != "postgres" {
		return
	}
	if err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm`).Error; err != nil {
		slog.Warn("search: pg_trgm unavailable; fuzzy search disabled", "err", err)
	}
}

// Indexes are the trigram indexes on songs (run with base.EnsureIndexes).
func Indexes(dialect string) []string {
	if dialect != "postgres" {
		return nil
	}
	return []string{
		`CREATE INDEX IF NOT EXISTS idx_songs_title_trgm ON songs USING gin (LOWER(title) gin_trgm_ops)`,
		`CREATE INDEX IF NOT EXISTS idx_songs_artist_trgm ON songs USING gin (LOWER(artist) gin_trgm_ops)`,
	}
}

func hasTrigram(db *gorm.DB) bool {
	if db.Dialector.Name() != "postgres" {
		return false
	}
	var n int64
	db.Raw(`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_trgm'`).Scan(&n)
	return n > 0
}

// Search
//
//	?q=       required
//	?source=  auto (default: howwe when GOOGLE_CSE_KEY is set, else local) | howwe | local
//	?page=&limit=  (howwe: fixed 10 per page, max 10 pages — Google's limits)
func (s *SearchService) Search(c *fiber.Ctx) error {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		return base.API_ERROR(c, "q is required")
	}
	if len(q) > 200 {
		return base.API_ERROR(c, "q is too long")
	}
	switch src := c.Query("source", "auto"); src {
	case "local":
		return s.local(c, q)
	case "howwe":
		if !s.Howwe.Enabled() {
			return base.API_FAIL(c, fiber.StatusNotImplemented, ErrNotConfigured.Error())
		}
		return s.howwe(c, q)
	case "auto":
		if s.Howwe.Enabled() {
			return s.howwe(c, q)
		}
		return s.local(c, q)
	default:
		return base.API_ERROR(c, "source must be one of: auto, howwe, local")
	}
}

// local searches the crawled catalog: every word must match the title or
// artist; exact and prefix title matches rank first, then popularity. If
// nothing matches and pg_trgm is available, it retries fuzzily (typos).
func (s *SearchService) local(c *fiber.Ctx, q string) error {
	p := base.ParsePage(c)
	lq := strings.ToLower(q)

	where := s.DB.Model(&song.Song{})
	for _, w := range strings.Fields(lq) {
		like := "%" + likeEscape(w) + "%"
		where = where.Where(`(LOWER(title) LIKE ? ESCAPE '\' OR LOWER(artist) LIKE ? ESCAPE '\')`, like, like)
	}
	// clause.OrderBy, not gorm.Expr: Order() silently drops a bare Expr.
	order := orderBy(`CASE WHEN LOWER(title) = ? THEN 0 WHEN LOWER(title) LIKE ? ESCAPE '\' THEN 1 WHEN LOWER(artist) = ? THEN 2 ELSE 3 END, votes IS NULL, votes DESC, id DESC`,
		lq, likeEscape(lq)+"%", lq)
	fuzzy := false

	var total int64
	if err := where.Count(&total).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "search failed")
	}
	if total == 0 && s.trigram {
		fuzzy = true
		where = s.DB.Model(&song.Song{}).Where(`word_similarity(?, LOWER(title || ' ' || artist)) >= 0.6`, lq)
		order = orderBy(`word_similarity(?, LOWER(title || ' ' || artist)) DESC, votes IS NULL, votes DESC`, lq)
		if err := where.Count(&total).Error; err != nil {
			return base.API_FAIL(c, fiber.StatusInternalServerError, "search failed")
		}
	}

	songs := []song.Song{}
	if total > 0 {
		if err := p.Apply(where).Order(order).Find(&songs).Error; err != nil {
			return base.API_FAIL(c, fiber.StatusInternalServerError, "search failed")
		}
	}
	return c.JSON(base.APIResponse{
		Data:       songs,
		Pagination: p.Controls(total, len(songs)),
		Meta:       fiber.Map{"source": "local", "query": q, "fuzzy": fuzzy},
	})
}

type howweHit struct {
	Kind    string     `json:"kind"` // song | artist | video | news | page
	Title   string     `json:"title"`
	Link    string     `json:"link"`
	Snippet string     `json:"snippet"`
	SongID  *uint      `json:"songId,omitempty"`
	Song    *song.Song `json:"song,omitempty"`
}

// howwe proxies howwe.ug's own search and joins song hits to the catalog.
// Songs the catalog doesn't have yet are added (their song page is then
// fetched by the next crawl), so search also feeds the crawler.
func (s *SearchService) howwe(c *fiber.Ctx, q string) error {
	page := base.ParsePage(c).Page
	res, fromCache, err := s.Howwe.Search(c.UserContext(), q, page)
	if err != nil {
		return base.API_FAIL(c, fiber.StatusBadGateway, err.Error())
	}

	hits := make([]howweHit, 0, len(res.Items))
	var refs []scraper.SongRef
	var ids []uint
	for _, it := range res.Items {
		h := howweHit{Title: it.Title, Link: it.Link, Snippet: it.Snippet, Kind: kindOf(it.Link)}
		if u, err := url.Parse(it.Link); err == nil {
			if ref, ok := scraper.ParseSongPath(u.Path); ok {
				id := uint(ref.ID)
				h.Kind, h.SongID = "song", &id
				ids = append(ids, id)
				if ref.Title, ref.Artist = splitSongTitle(it.Title); ref.Title != "" {
					refs = append(refs, ref)
				}
			}
		}
		hits = append(hits, h)
	}

	if len(ids) > 0 {
		var have []uint
		s.DB.Model(&song.Song{}).Where("id IN ?", ids).Pluck("id", &have)
		known := make(map[uint]bool, len(have))
		for _, id := range have {
			known[id] = true
		}
		var fresh []scraper.SongRef
		for _, r := range refs {
			if !known[uint(r.ID)] {
				fresh = append(fresh, r)
			}
		}
		if len(fresh) > 0 {
			if _, err := song.UpsertListings(s.DB, s.BaseURL, fresh, time.Now().UTC()); err != nil {
				slog.Warn("search: store new songs", "err", err)
			}
		}
		var songs []song.Song
		s.DB.Where("id IN ?", ids).Find(&songs)
		byID := make(map[uint]*song.Song, len(songs))
		for i := range songs {
			byID[songs[i].ID] = &songs[i]
		}
		for i := range hits {
			if hits[i].SongID != nil {
				hits[i].Song = byID[*hits[i].SongID]
			}
		}
	}

	total := min(res.Total, googleMaxRes)
	p := base.PageRequest{Page: page, Limit: pageSize}
	return c.JSON(base.APIResponse{
		Data:       hits,
		Pagination: p.Controls(total, len(hits)),
		Meta:       fiber.Map{"source": "howwe", "query": q, "engine": s.Howwe.CX, "cached": fromCache, "totalReported": res.Total},
	})
}

// splitSongTitle turns "Yeye by Kalifah AgaNaga - MP3 Download, Audio
// Download - Howwe.ug" (howwe's song page <title>) into title and artist.
func splitSongTitle(t string) (title, artist string) {
	if i := strings.Index(t, " - MP3"); i >= 0 {
		t = t[:i]
	}
	i := strings.LastIndex(t, " by ")
	if i <= 0 {
		return "", ""
	}
	return strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+4:])
}

func kindOf(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "page"
	}
	switch p := u.Path; {
	case strings.HasPrefix(p, "/song/"):
		return "song"
	case strings.HasPrefix(p, "/watch/") || strings.HasPrefix(p, "/video"):
		return "video"
	case strings.HasPrefix(p, "/news"):
		return "news"
	case strings.Count(strings.Trim(p, "/"), "/") == 0 && p != "/" && !strings.Contains(p, "."):
		return "artist"
	}
	return "page"
}

func orderBy(sql string, vars ...any) clause.OrderBy {
	return clause.OrderBy{Expression: clause.Expr{SQL: sql, Vars: vars, WithoutParentheses: true}}
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
