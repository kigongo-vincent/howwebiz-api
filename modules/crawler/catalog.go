package crawler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/genre"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CatalogOptions bound one catalog run. 0 means unlimited. Work left over
// is resumed by the next run from the stored cursor.
type CatalogOptions struct {
	MaxPages      int  `json:"maxPages"`      // listing pages (genre + /audio/new)
	MaxProbes     int  `json:"maxProbes"`     // song ids probed directly
	MaxDetails    int  `json:"maxDetails"`    // song pages fetched for audio URL etc.
	RefreshGenres bool `json:"refreshGenres"` // start a genre re-walk now (once synced)
	Reset         bool `json:"reset"`         // restart from the first genre / id 1
}

func (o CatalogOptions) Validate() error {
	if o.MaxPages < 0 || o.MaxProbes < 0 || o.MaxDetails < 0 {
		return errors.New("maxPages, maxProbes and maxDetails must be >= 0 (0 = unlimited)")
	}
	return nil
}

func CatalogDefaultsFromEnv() CatalogOptions {
	return CatalogOptions{
		MaxPages:   base.EnvInt("CATALOG_MAX_PAGES", 0),
		MaxProbes:  base.EnvInt("CATALOG_MAX_PROBES", 5000),
		MaxDetails: base.EnvInt("CATALOG_MAX_DETAILS", 10000),
	}
}

const (
	catalogCursorName = "catalog"
	genresPath        = "/genres"
	newestPath        = "/audio/new" // newest-first: used for incremental sync
	sweepChunk        = 200
	// Ids that were empty are retried after this long (songs can be
	// restored or ids reused).
	probeRetryAfter = 30 * 24 * time.Hour
)

func genreRefresh() time.Duration { return base.EnvDuration("CATALOG_GENRE_REFRESH", 7*24*time.Hour) }

// budget counts down; created from 0 it is unlimited.
type budget struct {
	left      int
	unlimited bool
}

func newBudget(n int) *budget { return &budget{left: n, unlimited: n == 0} }
func (b *budget) ok() bool    { return b.unlimited || b.left > 0 }
func (b *budget) take(n int) {
	if !b.unlimited {
		b.left -= n
	}
}

// StartCatalog launches a full-catalog crawl step in the background.
func (e *Engine) StartCatalog(trigger string, opts CatalogOptions) (*CrawlRun, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return e.Launch(trigger, KindCatalog, opts, func(ctx context.Context, st *RunState) {
		if err := e.runCatalog(ctx, opts, st); err != nil && ctx.Err() == nil {
			st.fail("catalog: %v", err)
		}
	})
}

func LoadCursor(db *gorm.DB) (CatalogCursor, error) {
	c := CatalogCursor{Name: catalogCursorName}
	err := db.Where("name = ?", catalogCursorName).Limit(1).Find(&c).Error
	if c.Phase == "" {
		c = freshCursor()
	}
	return c, err
}

func freshCursor() CatalogCursor {
	return CatalogCursor{Name: catalogCursorName, Phase: PhaseGenres, SweepNextID: 1}
}

func (e *Engine) saveCursor(c *CatalogCursor) error {
	c.UpdatedAt = time.Now().UTC()
	return e.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(c).Error
}

func (e *Engine) runCatalog(ctx context.Context, opts CatalogOptions, st *RunState) error {
	cur, err := LoadCursor(e.DB)
	if err != nil {
		return err
	}
	if opts.Reset {
		cur = freshCursor()
	}
	pages, probes := newBudget(opts.MaxPages), newBudget(opts.MaxProbes)

	// 1. First full walk of every genre (resumes at Genre/GenrePage).
	if cur.Phase == PhaseGenres {
		finished, err := e.walkGenres(ctx, &cur, pages, st)
		if err != nil {
			return err
		}
		if finished {
			cur.Phase = PhaseSweep
			if cur.SweepMaxID, err = song.MaxID(e.DB); err != nil {
				return err
			}
			if err := e.saveCursor(&cur); err != nil {
				return err
			}
			e.log.Info("catalog: genre walk complete", "genres", cur.GenresTotal, "sweepMaxId", cur.SweepMaxID)
		}
	}

	if cur.Phase == PhaseSynced && ctx.Err() == nil {
		// 2a. New songs: /audio/new is newest-first, so stop at the first
		// page that adds nothing.
		if _, err := e.walkPages(ctx, newestPath, "", 1, pages, true, nil, st); err != nil {
			return err
		}
		now := time.Now().UTC()
		cur.LastSyncAt = &now

		// 2b. Periodic genre re-walk (genre membership, songs missed by 2a).
		due := cur.GenreWalkDoneAt == nil || now.Sub(*cur.GenreWalkDoneAt) > genreRefresh()
		if cur.Genre != "" || due || opts.RefreshGenres {
			if _, err := e.walkGenres(ctx, &cur, pages, st); err != nil {
				return err
			}
		}

		// 2c. Sweep ids above the previous maximum.
		maxID, err := song.MaxID(e.DB)
		if err != nil {
			return err
		}
		if maxID > cur.SweepMaxID {
			cur.SweepNextID, cur.SweepMaxID, cur.Phase = cur.SweepMaxID+1, maxID, PhaseSweep
		}
		if err := e.saveCursor(&cur); err != nil {
			return err
		}
	}

	// 3. Probe ids no listing showed.
	if cur.Phase == PhaseSweep && ctx.Err() == nil {
		if err := e.sweep(ctx, &cur, probes, st); err != nil {
			return err
		}
	}

	// 4. Song pages (audio URL, album, rating...) for everything due,
	// never-fetched first.
	if ctx.Err() == nil {
		due, err := song.DetailBacklog(e.DB, opts.MaxDetails, time.Now().UTC())
		if err != nil {
			return err
		}
		st.add(func(s *Stats) { s.DetailsDue = len(due) })
		e.fetchDetails(ctx, due, st)
	}
	return nil
}

// walkGenres continues (or starts) a walk over every genre, in name order,
// from cur.Genre/cur.GenrePage. finished=true once the last genre is done.
func (e *Engine) walkGenres(ctx context.Context, cur *CatalogCursor, pages *budget, st *RunState) (finished bool, err error) {
	body, err := e.client.Get(ctx, genresPath)
	if err != nil {
		return false, fmt.Errorf("genres index: %w", err)
	}
	st.add(func(s *Stats) { s.PagesFetched++ })
	gs, err := scraper.ParseGenres(body)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if err := genre.Save(e.DB, gs, now); err != nil {
		return false, err
	}
	slices.SortFunc(gs, func(a, b scraper.Genre) int { return strings.Compare(a.Name, b.Name) })
	cur.GenresTotal = len(gs)

	if cur.Genre == "" { // start a new walk
		cur.Genre, cur.GenrePage, cur.GenresDone, cur.GenreWalkStartedAt = gs[0].Name, 1, 0, &now
	}
	// Resume at the stored genre, or the next one if it was removed.
	start := sort.Search(len(gs), func(i int) bool { return gs[i].Name >= cur.Genre })
	if start < len(gs) && gs[start].Name != cur.Genre {
		cur.GenrePage = 1
	}

	for i := start; i < len(gs); i++ {
		g := gs[i]
		cur.Genre = g.Name
		if cur.GenrePage < 1 {
			cur.GenrePage = 1
		}
		done, err := e.walkPages(ctx, g.Path, g.Name, cur.GenrePage, pages, false, func(next int) error {
			cur.GenrePage = next
			return e.saveCursor(cur)
		}, st)
		if err != nil {
			return false, fmt.Errorf("genre %s: %w", g.Name, err)
		}
		if !done {
			return false, e.saveCursor(cur) // budget or ctx; resume here
		}
		cur.GenresDone = i + 1
		cur.GenrePage = 1
		st.add(func(s *Stats) { s.GenresCompleted++ })
		e.log.Info("catalog: genre complete", "genre", g.Name, "siteCount", g.SongCount, "done", cur.GenresDone, "of", len(gs))
	}
	done := time.Now().UTC()
	cur.Genre, cur.GenreWalkDoneAt = "", &done
	return true, e.saveCursor(cur)
}

// walkPages walks a paginated song grid from startPage. finished=true when
// its last page was reached. genreName (if set) is recorded on each song.
// With stopWhenNoNew it stops at the first page that adds no new song.
// afterPage is called with the next page number after each stored page.
func (e *Engine) walkPages(ctx context.Context, listPath, genreName string, startPage int, pages *budget,
	stopWhenNoNew bool, afterPage func(next int) error, st *RunState) (finished bool, err error) {
	prevHash := ""
	for page := startPage; pages.ok(); page++ {
		if ctx.Err() != nil {
			return false, nil
		}
		path := listPath
		if page > 1 {
			path = fmt.Sprintf("%s/page/%d", listPath, page)
		}
		body, err := e.client.Get(ctx, path)
		if err != nil {
			return false, fmt.Errorf("page %d: %w", page, err)
		}
		pages.take(1)
		st.add(func(s *Stats) { s.PagesFetched++ })

		lp, err := scraper.ParseListing(body, listPath, page)
		if err != nil {
			return false, fmt.Errorf("page %d: %w", page, err)
		}
		// Past the end howwe.ug repeats the last page instead of 404ing.
		hash := scraper.Fingerprint(lp.Songs)
		if len(lp.Songs) == 0 || hash == prevHash {
			return true, nil
		}
		prevHash = hash
		for i := range lp.Songs {
			lp.Songs[i].Genre = genreName
		}

		now := time.Now().UTC()
		changed, err := e.pageChanged(e.client.BaseURL+path, hash, now)
		if err != nil {
			return false, err
		}
		inserted := 0
		if changed {
			us, err := song.UpsertListings(e.DB, e.client.BaseURL, lp.Songs, now)
			if err != nil {
				return false, err
			}
			inserted = us.Inserted
			st.add(func(s *Stats) {
				s.SongsInserted += us.Inserted
				s.SongsUpdated += us.Updated
				s.SongsUnchanged += us.Unchanged
			})
		} else {
			st.add(func(s *Stats) { s.PagesUnchanged++; s.SongsUnchanged += len(lp.Songs) })
		}
		st.add(func(s *Stats) { s.SongsSeen += len(lp.Songs) })

		if afterPage != nil {
			if err := afterPage(page + 1); err != nil {
				return false, err
			}
		}
		if !lp.HasNext {
			return true, nil
		}
		if stopWhenNoNew && inserted == 0 {
			return false, nil
		}
		if page%250 == 0 {
			e.log.Info("catalog: progress", "list", listPath, "page", page)
		}
	}
	return false, nil
}

func (e *Engine) sweep(ctx context.Context, cur *CatalogCursor, probes *budget, st *RunState) error {
	for cur.SweepNextID <= cur.SweepMaxID && probes.ok() && ctx.Err() == nil {
		from := cur.SweepNextID
		to := min(from+sweepChunk-1, cur.SweepMaxID)

		known, err := song.KnownIDs(e.DB, from, to)
		if err != nil {
			return err
		}
		var recentMissing []uint
		if err := e.DB.Model(&SongProbe{}).
			Where("id BETWEEN ? AND ? AND checked_at > ?", from, to, time.Now().UTC().Add(-probeRetryAfter)).
			Pluck("id", &recentMissing).Error; err != nil {
			return err
		}
		skip := make(map[uint]bool, len(recentMissing))
		for _, id := range recentMissing {
			skip[id] = true
		}

		var todo []uint
		for id := from; id <= to; id++ {
			if known[id] {
				continue
			}
			if skip[id] {
				st.add(func(s *Stats) { s.ProbeSkipped++ })
				continue
			}
			if !probes.ok() {
				to = id - 1 // resume here next run
				break
			}
			todo = append(todo, id)
			probes.take(1)
		}

		e.pool(ctx, todo, func(id uint) { e.probe(ctx, id, st) })
		if ctx.Err() != nil {
			return nil // chunk incomplete; it is redone next run
		}
		cur.SweepNextID = to + 1
		if cur.SweepNextID > cur.SweepMaxID {
			now := time.Now().UTC()
			cur.Phase, cur.SweepDoneAt = PhaseSynced, &now
			e.log.Info("catalog: id sweep complete", "maxId", cur.SweepMaxID)
		}
		if err := e.saveCursor(cur); err != nil {
			return err
		}
	}
	return nil
}

// probe fetches a song by id alone and stores it if it exists.
func (e *Engine) probe(ctx context.Context, id uint, st *RunState) {
	pageURL := e.client.BaseURL + scraper.ProbePath(int64(id))
	body, err := e.client.Get(ctx, scraper.ProbePath(int64(id)))
	if err != nil {
		if ctx.Err() == nil {
			st.fail("probe %d: %v", id, err)
		}
		return
	}
	st.add(func(s *Stats) { s.Probed++ })
	now := time.Now().UTC()
	d, err := scraper.ParseSong(body, pageURL)
	if errors.Is(err, scraper.ErrNoSong) {
		if err := e.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&SongProbe{ID: id, CheckedAt: now}).Error; err != nil {
			st.fail("probe %d: %v", id, err)
		}
		return
	}
	if err != nil {
		st.fail("probe %d: %v", id, err)
		return
	}
	if err := song.SaveDiscovered(e.DB, e.client.BaseURL, id, d, e.Cfg.DetailTTL, now); err != nil {
		st.fail("probe %d: save: %v", id, err)
		return
	}
	e.DB.Delete(&SongProbe{}, id)
	st.add(func(s *Stats) { s.ProbeFound++; s.SongsInserted++ })
}
