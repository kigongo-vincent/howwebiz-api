package crawler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/chart"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrBusy = errors.New("a crawl is already running")

// Config is read from the environment (see .env.example).
type Config struct {
	BaseURL         string
	UserAgent       string
	RequestDelay    time.Duration
	RequestTimeout  time.Duration
	DetailWorkers   int
	DetailTTL       time.Duration // base revisit interval for song pages
	DetailMaxPerRun int           // 0 = unlimited; the rest are picked up next run
	Defaults        Options
}

func ConfigFromEnv() Config {
	return Config{
		BaseURL:         strings.TrimRight(base.Env("CRAWL_BASE_URL", "https://www.howwe.ug"), "/"),
		UserAgent:       base.Env("CRAWL_USER_AGENT", "howwebiz-api-crawler/1.0"),
		RequestDelay:    base.EnvDuration("CRAWL_REQUEST_DELAY", 500*time.Millisecond),
		RequestTimeout:  base.EnvDuration("CRAWL_REQUEST_TIMEOUT", 20*time.Second),
		DetailWorkers:   max(1, base.EnvInt("CRAWL_DETAIL_WORKERS", 4)),
		DetailTTL:       base.EnvDuration("CRAWL_DETAIL_TTL", 24*time.Hour),
		DetailMaxPerRun: base.EnvInt("CRAWL_DETAIL_MAX_PER_RUN", 500),
		Defaults: Options{
			Targets:      base.EnvList("CRAWL_TARGETS", []string{"hot100", "audio-new"}),
			Pages:        base.EnvInt("CRAWL_PAGES", 3),
			FetchDetails: true,
		},
	}
}

func (o Options) Validate() error {
	if len(o.Targets) == 0 {
		return errors.New("targets: at least one target is required")
	}
	for _, t := range o.Targets {
		if _, ok := chart.Lists[t]; !ok {
			return fmt.Errorf("targets: unknown target %q (valid: %s)", t, strings.Join(chart.ListNames(), ", "))
		}
	}
	if o.Pages < 1 || o.Pages > 500 {
		return errors.New("pages: must be between 1 and 500")
	}
	return nil
}

// Engine runs crawls. One crawl at a time per process.
type Engine struct {
	DB     *gorm.DB
	Cfg    Config
	client *scraper.Client
	ctx    context.Context // server lifetime; cancelling it stops a crawl
	log    *slog.Logger
	*Runner
}

func NewEngine(ctx context.Context, db *gorm.DB, cfg Config) *Engine {
	return &Engine{
		DB:     db,
		Cfg:    cfg,
		client: scraper.NewClient(cfg.BaseURL, cfg.UserAgent, cfg.RequestDelay, cfg.RequestTimeout),
		ctx:    ctx,
		log:    slog.Default().With("module", "crawler"),
		Runner: NewRunner(ctx, db, slog.Default().With("module", "crawler"), KindLists, KindCatalog),
	}
}

// Start launches a list crawl in the background and returns its run.
func (e *Engine) Start(trigger string, opts Options) (*CrawlRun, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return e.Launch(trigger, KindLists, opts, func(ctx context.Context, st *RunState) {
		for _, t := range opts.Targets {
			if ctx.Err() != nil {
				break
			}
			if err := e.crawlList(ctx, t, opts, st); err != nil {
				e.log.Warn("target failed", "target", t, "err", err)
				st.fail("%s: %v", t, err)
			}
		}
		st.add(func(s *Stats) { s.SongsSeen = len(st.seen) })
		if opts.FetchDetails && ctx.Err() == nil {
			e.crawlDetails(ctx, opts.Force, st)
		}
	})
}

// pageChanged records a listing page's fingerprint and reports whether it
// differs from the previous crawl.
func (e *Engine) pageChanged(url, hash string, now time.Time) (bool, error) {
	return PageChanged(e.DB, url, hash, now)
}

// PageChanged records url's content fingerprint and reports whether it
// differs from the last one seen (shared by every source's crawler).
func PageChanged(db *gorm.DB, url, hash string, now time.Time) (bool, error) {
	var fp PageFingerprint
	if err := db.Where("url = ?", url).Limit(1).Find(&fp).Error; err != nil {
		return true, err
	}
	changed := fp.URL == "" || fp.Hash != hash
	next := PageFingerprint{URL: url, Hash: hash, CheckedAt: now, ChangedAt: fp.ChangedAt, UnchangedStreak: fp.UnchangedStreak + 1}
	if changed {
		next.ChangedAt, next.UnchangedStreak = now, 0
	}
	return changed, db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&next).Error
}

func (e *Engine) crawlList(ctx context.Context, list string, opts Options, st *RunState) error {
	src := chart.Lists[list]
	now := time.Now().UTC()
	pages := 1
	if src.Paginated {
		pages = opts.Pages
	}

	var entries []scraper.ChartEntry
	for page := 1; page <= pages; page++ {
		path := src.Path
		if page > 1 {
			path = fmt.Sprintf("%s/page/%d", src.Path, page)
		}
		body, err := e.client.Get(ctx, path)
		if err != nil {
			if len(entries) == 0 {
				return err
			}
			st.fail("%s page %d: %v", list, page, err)
			pages = page - 1
			break
		}
		st.add(func(s *Stats) { s.PagesFetched++ })

		var pageEntries []scraper.ChartEntry
		hasNext := false
		if src.Paginated {
			lp, err := scraper.ParseListing(body, src.Path, page)
			if err != nil {
				return err
			}
			for _, s := range lp.Songs {
				pageEntries = append(pageEntries, scraper.ChartEntry{Position: len(entries) + len(pageEntries) + 1, Song: s})
			}
			hasNext = lp.HasNext
		} else if pageEntries, err = scraper.ParseHot100(body); err != nil {
			return err
		}
		if len(pageEntries) == 0 {
			if page == 1 {
				return errors.New("no songs found (page layout may have changed)")
			}
			pages = page - 1
			break
		}

		changed, err := e.pageChanged(e.client.BaseURL+path, scraper.Fingerprint(pageEntries), now)
		if err != nil {
			return err
		}
		refs := make([]scraper.SongRef, len(pageEntries))
		ids := make([]uint, len(pageEntries))
		for i, pe := range pageEntries {
			refs[i], ids[i] = pe.Song, uint(pe.Song.ID)
		}
		st.see(ids...)

		if !changed && !opts.Force {
			st.add(func(s *Stats) { s.PagesUnchanged++; s.SongsUnchanged += len(refs) })
			// Newest-first list: page 1 identical ⇒ nothing was added, so the
			// later pages are identical too. Reuse the stored snapshot.
			if page == 1 && src.NewestFirst {
				if latest, err := chart.Latest(e.DB, list); err == nil && latest != nil && latest.Pages >= pages {
					if err := chart.Touch(e.DB, latest.ID, now); err != nil {
						return err
					}
					var rest []uint
					e.DB.Model(&chart.Entry{}).Where("snapshot_id = ?", latest.ID).Pluck("song_id", &rest)
					st.see(rest...)
					st.add(func(s *Stats) { s.PagesSkipped += pages - 1; s.SnapshotsReused++ })
					e.log.Info("list unchanged, skipped remaining pages", "list", list, "skipped", pages-1)
					return nil
				}
			}
		} else {
			us, err := song.UpsertListings(e.DB, e.client.BaseURL, refs, now)
			if err != nil {
				return fmt.Errorf("save songs: %w", err)
			}
			st.add(func(s *Stats) {
				s.SongsInserted += us.Inserted
				s.SongsUpdated += us.Updated
				s.SongsUnchanged += us.Unchanged
			})
		}
		entries = append(entries, pageEntries...)
		if src.Paginated && !hasNext {
			pages = page
			break
		}
	}

	_, created, err := chart.Save(e.DB, list, pages, entries, now)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	st.add(func(s *Stats) {
		if created {
			s.SnapshotsCreated++
		} else {
			s.SnapshotsReused++
		}
	})
	e.log.Info("list crawled", "list", list, "entries", len(entries), "newSnapshot", created)
	return nil
}

func (e *Engine) crawlDetails(ctx context.Context, force bool, st *RunState) {
	ids := make([]uint, 0, len(st.seen))
	for id := range st.seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	due := ids
	if !force {
		var err error
		if due, err = song.DueForDetail(e.DB, ids, time.Now().UTC()); err != nil {
			st.fail("select due songs: %v", err)
			return
		}
	}
	deferred := 0
	if limit := e.Cfg.DetailMaxPerRun; limit > 0 && len(due) > limit {
		deferred = len(due) - limit
		due = due[:limit]
	}
	st.add(func(s *Stats) {
		s.DetailsDue = len(due) + deferred
		s.DetailsNotDue = len(ids) - len(due) - deferred
		s.DetailsDeferred = deferred
	})
	e.fetchDetails(ctx, due, st)
}

// fetchDetails visits song pages with DetailWorkers workers.
func (e *Engine) fetchDetails(ctx context.Context, ids []uint, st *RunState) {
	e.pool(ctx, ids, func(id uint) {
		changed, err := e.crawlDetail(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				st.fail("song %d: %v", id, err)
			}
			return
		}
		st.add(func(s *Stats) {
			s.DetailsFetched++
			if changed {
				s.DetailsChanged++
			} else {
				s.DetailsUnchanged++
			}
		})
	})
}

// pool runs fn over ids with DetailWorkers goroutines, stopping early if
// ctx is cancelled. The shared client delay still paces the requests.
func (e *Engine) pool(ctx context.Context, ids []uint, fn func(id uint)) {
	jobs := make(chan uint)
	var wg sync.WaitGroup
	for range e.Cfg.DetailWorkers {
		wg.Go(func() {
			for id := range jobs {
				fn(id)
			}
		})
	}
feed:
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
}

func (e *Engine) crawlDetail(ctx context.Context, id uint) (bool, error) {
	pageURL, err := song.PageURL(e.DB, id)
	if err != nil {
		return false, err
	}
	body, err := e.client.Get(ctx, strings.TrimPrefix(pageURL, e.client.BaseURL))
	if err != nil {
		return false, err
	}
	d, err := scraper.ParseSong(body, pageURL)
	if err != nil {
		return false, err
	}
	return song.SaveDetail(e.DB, id, d, e.Cfg.DetailTTL, time.Now().UTC())
}
