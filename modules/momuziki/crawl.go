package momuziki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	Kind       = "momuziki"
	cursorName = "catalog"
	siteName   = "momuziki.com"
)

type Config struct {
	Web             webguard.Config
	DetailTTL       time.Duration
	ProbeAhead      int // misses in a row that end the probe above the highest id
	ProbeRetryAfter time.Duration
	CategoryRefresh time.Duration
	MaxBrokenSongs  int // known songs in a row whose page has no data → layout changed, stop
	Defaults        Options
	Daily           bool
}

func ConfigFromEnv() Config {
	return Config{
		Web:             webguard.ConfigFromEnv("MOMUZIKI", "https://www.momuziki.com", time.Second),
		DetailTTL:       base.EnvDuration("MOMUZIKI_DETAIL_TTL", 24*time.Hour),
		ProbeAhead:      max(1, base.EnvInt("MOMUZIKI_PROBE_AHEAD", 25)),
		ProbeRetryAfter: base.EnvDuration("MOMUZIKI_PROBE_RETRY_AFTER", 30*24*time.Hour),
		CategoryRefresh: base.EnvDuration("MOMUZIKI_CATEGORY_REFRESH", 7*24*time.Hour),
		MaxBrokenSongs:  max(1, base.EnvInt("MOMUZIKI_MAX_BROKEN_SONG_PAGES", 5)),
		Daily:           base.EnvBool("MOMUZIKI_DAILY", true),
		Defaults: Options{
			MaxPages:   base.EnvInt("MOMUZIKI_MAX_PAGES", 0),
			MaxProbes:  base.EnvInt("MOMUZIKI_MAX_PROBES", 2000),
			MaxDetails: base.EnvInt("MOMUZIKI_MAX_DETAILS", 2000),
		},
	}
}

// Options bound one crawl run; 0 = unlimited. Leftovers resume next run.
type Options struct {
	MaxPages          int  `json:"maxPages"`   // category pages
	MaxProbes         int  `json:"maxProbes"`  // song.php ids probed (not listed anywhere)
	MaxDetails        int  `json:"maxDetails"` // song.php pages for listed songs (plays, cover)
	RefreshCategories bool `json:"refreshCategories"`
	Reset             bool `json:"reset"`
}

func (o Options) Validate() error {
	if o.MaxPages < 0 || o.MaxProbes < 0 || o.MaxDetails < 0 {
		return errors.New("maxPages, maxProbes and maxDetails must be >= 0 (0 = unlimited)")
	}
	return nil
}

type budget struct {
	left      int
	unlimited bool
}

func newBudget(n int) *budget { return &budget{left: n, unlimited: n == 0} }
func (b *budget) ok() bool    { return b.unlimited || b.left > 0 }
func (b *budget) take()       { b.left-- }

// Service is the momuziki.com source.
type Service struct {
	DB     *gorm.DB
	Cfg    Config
	Web    *webguard.Fetcher
	Runner *crawler.Runner
	base   *url.URL
}

func NewService(ctx context.Context, db *gorm.DB, cfg Config) *Service {
	b, _ := url.Parse(cfg.Web.BaseURL + "/")
	s := &Service{DB: db, Cfg: cfg, base: b,
		Web:    webguard.New(db, siteName, cfg.Web),
		Runner: crawler.NewRunner(ctx, db, slog.Default().With("module", "momuziki"), Kind)}
	s.Runner.OnResume(Kind, func(trigger string, raw json.RawMessage) (*crawler.CrawlRun, error) {
		var o Options
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		return s.StartCrawl(trigger, o)
	})
	return s
}

func (s *Service) loadCursor() Cursor {
	c := Cursor{Name: cursorName}
	s.DB.Where("name = ?", cursorName).Limit(1).Find(&c)
	if c.Phase == "" {
		c = Cursor{Name: cursorName, Phase: PhaseCategories, SweepNextID: 1}
	}
	return c
}

func (s *Service) saveCursor(c *Cursor) error {
	c.UpdatedAt = time.Now().UTC()
	return s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(c).Error
}

// StartCrawl runs the next step of the full crawl in the background.
func (s *Service) StartCrawl(trigger string, opts Options) (*crawler.CrawlRun, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if open, until := s.Web.CircuitOpen(); open {
		return nil, fmt.Errorf("%w until %s; POST /api/v1/momuziki/guard/reset to override", webguard.ErrCircuitOpen, until.Format(time.RFC3339))
	}
	return s.Runner.Launch(trigger, Kind, opts, func(ctx context.Context, st *crawler.RunState) {
		s.Web.StartRun()
		if err := s.run(ctx, opts, st); err != nil && ctx.Err() == nil {
			st.Fail("%v", err)
		}
	})
}

func fatal(err error) bool {
	return errors.Is(err, webguard.ErrBlocked) || errors.Is(err, webguard.ErrCircuitOpen)
}

func (s *Service) run(ctx context.Context, opts Options, st *crawler.RunState) error {
	cur := s.loadCursor()
	if opts.Reset {
		cur = Cursor{Name: cursorName, Phase: PhaseCategories, SweepNextID: 1}
	}
	pages, probes := newBudget(opts.MaxPages), newBudget(opts.MaxProbes)

	switch cur.Phase {
	case PhaseCategories:
		done, err := s.walkCategories(ctx, &cur, pages, false, st)
		if err != nil {
			return err
		}
		if done {
			cur.Phase = PhaseSweep
			if err := s.saveCursor(&cur); err != nil {
				return err
			}
		}
	case PhaseSynced:
		// New songs first: every category is newest first, so stop at the
		// first page that adds nothing.
		if _, err := s.walkCategories(ctx, &cur, pages, true, st); err != nil {
			return err
		}
		now := time.Now().UTC()
		cur.LastSyncAt = &now
		due := cur.WalkDoneAt == nil || now.Sub(*cur.WalkDoneAt) > s.Cfg.CategoryRefresh
		if cur.CategoryID != 0 || due || opts.RefreshCategories {
			if _, err := s.walkCategories(ctx, &cur, pages, false, st); err != nil {
				return err
			}
		}
		if err := s.saveCursor(&cur); err != nil {
			return err
		}
	}

	if (cur.Phase == PhaseSweep || cur.Phase == PhaseSynced) && ctx.Err() == nil {
		if err := s.sweep(ctx, &cur, probes, st); err != nil {
			return err
		}
	}

	if ctx.Err() == nil {
		due, err := detailBacklog(s.DB, opts.MaxDetails, time.Now().UTC())
		if err != nil {
			return err
		}
		st.Add(func(x *crawler.Stats) { x.DetailsDue = len(due) })
		broken := 0
		for _, id := range due {
			if ctx.Err() != nil {
				return nil
			}
			changed, err := s.fetchSong(ctx, id)
			switch {
			case fatal(err):
				return err
			case errors.Is(err, ErrNoSong):
				broken++
				st.Fail("song %d: page has no data (removed?)", id)
				if broken >= s.Cfg.MaxBrokenSongs {
					return fmt.Errorf("stopping: %d listed songs in a row have empty pages (layout may have changed)", broken)
				}
				continue
			case err != nil:
				st.Fail("song %d: %v", id, err)
				continue
			}
			broken = 0
			st.Add(func(x *crawler.Stats) {
				x.DetailsFetched++
				if changed {
					x.DetailsChanged++
				} else {
					x.DetailsUnchanged++
				}
			})
		}
	}
	return nil
}

// walkCategories walks the categories. incremental: each from page 1 until
// a page adds nothing. Otherwise a full walk, resumed from cur.CategoryID /
// cur.Page; finished=true when every category was walked to its end.
func (s *Service) walkCategories(ctx context.Context, cur *Cursor, pages *budget, incremental bool, st *crawler.RunState) (finished bool, err error) {
	body, err := s.Web.Get(ctx, "/categories.php")
	if err != nil {
		return false, fmt.Errorf("categories: %w", err)
	}
	st.Add(func(x *crawler.Stats) { x.PagesFetched++ })
	cats, err := ParseCategories(body, s.base)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	rows := make([]Category, len(cats))
	for i, c := range cats {
		rows[i] = Category{ID: c.ID, Name: c.Name, ImageURL: c.ImageURL, UpdatedAt: now}
	}
	if err := s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows).Error; err != nil {
		return false, err
	}
	slices.SortFunc(cats, func(a, b CategoryRef) int { return int(a.ID) - int(b.ID) })
	cur.CategoriesTotal = len(cats)

	if incremental {
		for _, c := range cats {
			if _, err := s.walkCategory(ctx, c.ID, 1, pages, true, nil, st); err != nil {
				if fatal(err) {
					return false, err
				}
				st.Fail("category %d: %v", c.ID, err)
			}
		}
		return true, nil
	}

	if cur.CategoryID == 0 {
		cur.CategoryID, cur.Page, cur.CategoriesDone, cur.WalkStartedAt = cats[0].ID, 1, 0, &now
	}
	for i, c := range cats {
		if c.ID < cur.CategoryID {
			continue
		}
		if c.ID != cur.CategoryID { // resumed category disappeared, or moved on
			cur.CategoryID, cur.Page = c.ID, 1
		}
		done, err := s.walkCategory(ctx, c.ID, max(cur.Page, 1), pages, false, func(next int) error {
			cur.Page = next
			return s.saveCursor(cur)
		}, st)
		if err != nil {
			if fatal(err) {
				return false, err
			}
			st.Fail("category %d: %v", c.ID, err)
		}
		if !done && err == nil {
			return false, s.saveCursor(cur) // out of budget: resume here
		}
		cur.CategoriesDone, cur.Page = i+1, 1
	}
	end := time.Now().UTC()
	cur.CategoryID, cur.WalkDoneAt = 0, &end
	return true, s.saveCursor(cur)
}

func (s *Service) walkCategory(ctx context.Context, catID uint, page int, pages *budget, incremental bool,
	afterPage func(next int) error, st *crawler.RunState) (done bool, err error) {
	for ; pages.ok(); page++ {
		if ctx.Err() != nil {
			return false, nil
		}
		path := pagePath(catID, page)
		body, err := s.Web.Get(ctx, path)
		if err != nil {
			return false, err
		}
		pages.take()
		st.Add(func(x *crawler.Stats) { x.PagesFetched++ })
		rows, err := ParseRows(body, s.base)
		if err != nil {
			return false, err
		}
		if len(rows) == 0 { // past the last page
			return true, nil
		}
		now := time.Now().UTC()
		changed, err := crawler.PageChanged(s.DB, s.Cfg.Web.BaseURL+path, scraper.Fingerprint(rows), now)
		if err != nil {
			return false, err
		}
		inserted := 0
		if changed {
			us, err := UpsertRows(s.DB, s.Cfg.Web.BaseURL, rows, catID, now)
			if err != nil {
				return false, err
			}
			inserted = us.Inserted
			st.Add(func(x *crawler.Stats) {
				x.SongsInserted += us.Inserted
				x.SongsUpdated += us.Updated
				x.SongsUnchanged += us.Unchanged
			})
		} else {
			st.Add(func(x *crawler.Stats) { x.PagesUnchanged++; x.SongsUnchanged += len(rows) })
		}
		st.Add(func(x *crawler.Stats) { x.SongsSeen += len(rows) })
		if afterPage != nil {
			if err := afterPage(page + 1); err != nil {
				return false, err
			}
		}
		if incremental && inserted == 0 {
			return true, nil
		}
	}
	return false, nil
}

// sweep probes song ids no category listed: first 1..highest known id,
// then onward until ProbeAhead misses in a row (new, uncategorized uploads).
func (s *Service) sweep(ctx context.Context, cur *Cursor, probes *budget, st *crawler.RunState) error {
	top := maxID(s.DB)
	for cur.SweepNextID <= top && probes.ok() && ctx.Err() == nil {
		from, to := cur.SweepNextID, min(cur.SweepNextID+199, top)
		var known, missing []uint
		s.DB.Model(&Song{}).Where("id BETWEEN ? AND ?", from, to).Pluck("id", &known)
		s.DB.Model(&Probe{}).Where("id BETWEEN ? AND ? AND checked_at > ?", from, to, time.Now().UTC().Add(-s.Cfg.ProbeRetryAfter)).Pluck("id", &missing)
		skip := map[uint]bool{}
		for _, id := range append(known, missing...) {
			skip[id] = true
		}
		for id := from; id <= to; id++ {
			if skip[id] {
				continue
			}
			if !probes.ok() || ctx.Err() != nil {
				return s.saveCursor(cur)
			}
			if _, err := s.probe(ctx, id, probes, st); err != nil {
				return err
			}
			cur.SweepNextID = id + 1
		}
		cur.SweepNextID = to + 1
		if err := s.saveCursor(cur); err != nil {
			return err
		}
	}
	if cur.SweepNextID <= top || !probes.ok() || ctx.Err() != nil {
		return nil
	}
	// Ahead of the highest id: stop after ProbeAhead misses in a row.
	misses := 0
	for id := max(cur.SweepNextID, top+1); misses < s.Cfg.ProbeAhead && probes.ok() && ctx.Err() == nil; id++ {
		found, err := s.probe(ctx, id, probes, st)
		if err != nil {
			return err
		}
		if found {
			misses, cur.SweepNextID = 0, id+1
		} else {
			misses++
		}
	}
	if cur.Phase == PhaseSweep && misses >= s.Cfg.ProbeAhead {
		now := time.Now().UTC()
		cur.Phase, cur.SweepDoneAt = PhaseSynced, &now
	}
	return s.saveCursor(cur)
}

func (s *Service) probe(ctx context.Context, id uint, probes *budget, st *crawler.RunState) (found bool, err error) {
	body, err := s.Web.Get(ctx, fmt.Sprintf("/song.php?id=%d", id))
	if err != nil {
		if fatal(err) {
			return false, err
		}
		st.Fail("probe %d: %v", id, err)
		return false, nil
	}
	probes.take()
	st.Add(func(x *crawler.Stats) { x.Probed++ })
	now := time.Now().UTC()
	p, err := ParseSong(body, s.base)
	if errors.Is(err, ErrNoSong) {
		return false, s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&Probe{ID: id, CheckedAt: now}).Error
	}
	if err != nil {
		st.Fail("probe %d: %v", id, err)
		return false, nil
	}
	if _, err := SaveSongPage(s.DB, s.Cfg.Web.BaseURL, id, p, s.Cfg.DetailTTL, now); err != nil {
		return false, err
	}
	s.DB.Delete(&Probe{}, id)
	st.Add(func(x *crawler.Stats) { x.ProbeFound++; x.SongsInserted++ })
	return true, nil
}

func (s *Service) fetchSong(ctx context.Context, id uint) (bool, error) {
	body, err := s.Web.Get(ctx, fmt.Sprintf("/song.php?id=%d", id))
	if err != nil {
		return false, err
	}
	p, err := ParseSong(body, s.base)
	if err != nil {
		return false, err
	}
	return SaveSongPage(s.DB, s.Cfg.Web.BaseURL, id, p, s.Cfg.DetailTTL, time.Now().UTC())
}
