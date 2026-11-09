package sitecrawl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const KindPrefix = "site:" // CrawlRun.Kind = "site:<name>"

type Options struct {
	MaxPages  int  `json:"maxPages"`  // listing pages; 0 = unlimited
	MaxProbes int  `json:"maxProbes"` // song pages by id; 0 = unlimited
	Refresh   bool `json:"refresh"`   // start a full listing re-walk now (once synced)
	Reset     bool `json:"reset"`
}

func (o Options) Validate() error {
	if o.MaxPages < 0 || o.MaxProbes < 0 {
		return errors.New("maxPages and maxProbes must be >= 0 (0 = unlimited)")
	}
	return nil
}

type Config struct {
	Web             webguard.Config
	ProbeAhead      int
	ProbeRetryAfter time.Duration
	Refresh         time.Duration
	Defaults        Options
	Daily           bool
	// SearchEnrich: song pages fetched per live search for results the
	// search page lists without audio (Prober sites).
	SearchEnrich int
}

// ConfigFromEnv reads <PREFIX>_* (e.g. PEARLTUNES_DELAY).
func ConfigFromEnv(prefix, baseURL string) Config {
	return Config{
		Web:             webguard.ConfigFromEnv(prefix, baseURL, time.Second),
		ProbeAhead:      max(1, base.EnvInt(prefix+"_PROBE_AHEAD", 25)),
		ProbeRetryAfter: base.EnvDuration(prefix+"_PROBE_RETRY_AFTER", 30*24*time.Hour),
		Refresh:         base.EnvDuration(prefix+"_REFRESH", 7*24*time.Hour),
		Daily:           base.EnvBool(prefix+"_DAILY", true),
		SearchEnrich:    base.EnvInt(prefix+"_SEARCH_ENRICH", 5),
		Defaults: Options{
			MaxPages:  base.EnvInt(prefix+"_MAX_PAGES", 0),
			MaxProbes: base.EnvInt(prefix+"_MAX_PROBES", 3000),
		},
	}
}

const (
	PhaseListing = "listing"
	PhaseSweep   = "sweep"
	PhaseSynced  = "synced"
)

// Cursor is a site's resumable crawl position.
type Cursor struct {
	Site        string     `json:"site" gorm:"primaryKey;size:32"`
	Phase       string     `json:"phase" gorm:"size:16"`
	NextPage    int        `json:"nextPage"`
	Walking     bool       `json:"walking"` // a full listing walk is in progress
	WalkDoneAt  *time.Time `json:"walkDoneAt"`
	SweepNextID int        `json:"sweepNextId"`
	ListMaxID   int        `json:"listMaxId"` // newest id seen on the listing (Prober sites)
	SweepDoneAt *time.Time `json:"sweepDoneAt"`
	LastSyncAt  *time.Time `json:"lastSyncAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

func (Cursor) TableName() string { return "sitecrawl_cursors" }

// Probe remembers ids whose page had no song.
type Probe struct {
	Site      string    `gorm:"primaryKey;size:32"`
	ID        int       `gorm:"primaryKey;autoIncrement:false"`
	CheckedAt time.Time `gorm:"not null"`
}

func (Probe) TableName() string { return "sitecrawl_probes" }

// Crawler crawls one Site.
type Crawler struct {
	DB       *gorm.DB
	Site     Site
	Cfg      Config
	Web      *webguard.Fetcher
	Runner   *crawler.Runner
	Ingester *catalog.Ingester
	log      *slog.Logger
}

func New(ctx context.Context, db *gorm.DB, site Site, cfg Config, in *catalog.Ingester) *Crawler {
	log := slog.Default().With("module", "sitecrawl", "site", site.Name())
	return &Crawler{DB: db, Site: site, Cfg: cfg, Ingester: in, log: log,
		Web:    webguard.New(db, site.Name(), cfg.Web),
		Runner: crawler.NewRunner(ctx, db, log, KindPrefix+site.Name())}
}

func (c *Crawler) Kind() string { return KindPrefix + c.Site.Name() }

func (c *Crawler) cursor() Cursor {
	cur := Cursor{Site: c.Site.Name()}
	c.DB.Where("site = ?", c.Site.Name()).Limit(1).Find(&cur)
	if cur.Phase == "" {
		cur = Cursor{Site: c.Site.Name(), Phase: PhaseListing, NextPage: 1, Walking: true, SweepNextID: 1}
	}
	return cur
}

func (c *Crawler) save(cur *Cursor) error {
	cur.UpdatedAt = time.Now().UTC()
	return c.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(cur).Error
}

func fatal(err error) bool {
	return errors.Is(err, webguard.ErrBlocked) || errors.Is(err, webguard.ErrCircuitOpen)
}

type budget struct {
	left      int
	unlimited bool
}

func newBudget(n int) *budget { return &budget{left: n, unlimited: n == 0} }
func (b *budget) ok() bool    { return b.unlimited || b.left > 0 }
func (b *budget) take()       { b.left-- }

// Start runs the next crawl step in the background.
func (c *Crawler) Start(trigger string, opts Options) (*crawler.CrawlRun, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if open, until := c.Web.CircuitOpen(); open {
		return nil, fmt.Errorf("%w until %s", webguard.ErrCircuitOpen, until.Format(time.RFC3339))
	}
	return c.Runner.Launch(trigger, c.Kind(), opts, func(ctx context.Context, st *crawler.RunState) {
		c.Web.StartRun()
		if err := c.run(ctx, opts, st); err != nil && ctx.Err() == nil {
			st.Fail("%v", err)
		}
	})
}

func (c *Crawler) DailySync() {
	if !c.Cfg.Daily {
		return
	}
	if open, _ := c.Web.CircuitOpen(); open {
		return
	}
	if _, err := c.Start("cron", c.Cfg.Defaults); err != nil {
		c.log.Warn("scheduled crawl not started", "err", err)
	}
}

func (c *Crawler) run(ctx context.Context, opts Options, st *crawler.RunState) error {
	cur := c.cursor()
	if opts.Reset {
		cur = Cursor{Site: c.Site.Name(), Phase: PhaseListing, NextPage: 1, Walking: true, SweepNextID: 1}
	}
	pages, probes := newBudget(opts.MaxPages), newBudget(opts.MaxProbes)
	_, prober := c.Site.(Prober)
	lister, isLister := c.Site.(Lister)

	// 1. Listing (sites that have one, and whose robots.txt allows it).
	if !isLister && cur.Phase == PhaseListing {
		cur.Phase, cur.Walking = PhaseSweep, false // nothing to walk
	}
	switch {
	case isLister && prober:
		// The catalog stores only songs with audio, and listing cards carry
		// none here: the listing is discovery only (page 1 → newest id),
		// and the sweep fetches song pages, which carry the mp3.
		if err := c.discover(ctx, lister, &cur, pages, st); err != nil {
			return err
		}
		if cur.Phase == PhaseListing {
			cur.Phase, cur.Walking, cur.NextPage = PhaseSweep, false, 1
		}
	case isLister && cur.Phase == PhaseListing:
		done, err := c.walk(ctx, lister, &cur, pages, false, st)
		if err != nil {
			return err
		}
		if done {
			now := time.Now().UTC()
			cur.Walking, cur.WalkDoneAt, cur.NextPage = false, &now, 1
			cur.Phase = PhaseSynced
			if prober {
				cur.Phase = PhaseSweep
			}
		}
	case isLister && cur.Phase != PhaseListing:
		if _, err := c.walk(ctx, lister, &Cursor{NextPage: 1}, pages, true, st); err != nil {
			return err
		}
		now := time.Now().UTC()
		due := cur.WalkDoneAt == nil || now.Sub(*cur.WalkDoneAt) > c.Cfg.Refresh
		if cur.Walking || due || opts.Refresh {
			if !cur.Walking {
				cur.Walking, cur.NextPage = true, 1
			}
			done, err := c.walk(ctx, lister, &cur, pages, false, st)
			if err != nil {
				return err
			}
			if done {
				end := time.Now().UTC()
				cur.Walking, cur.WalkDoneAt, cur.NextPage = false, &end, 1
			}
		}
	}
	if err := c.save(&cur); err != nil {
		return err
	}

	// 2. Sitemap: pages new or modified since they were last fetched.
	if sm, ok := c.Site.(Sitemapper); ok && ctx.Err() == nil {
		if err := c.sitemapPass(ctx, sm, probes, st); err != nil {
			return err
		}
	}

	// 3. Song pages by id.
	if prober && cur.Phase != PhaseListing && ctx.Err() == nil {
		if err := c.sweep(ctx, &cur, probes, st); err != nil {
			return err
		}
	} else if !prober && cur.Phase == PhaseSweep {
		cur.Phase = PhaseSynced
	}
	now := time.Now().UTC()
	cur.LastSyncAt = &now
	return c.save(&cur)
}

// discover reads listing page 1 of a Prober site: it records the newest
// id (so the sweep reaches it) and stores whatever the cards carry with
// audio. Ids listed there lose any "missing" mark, so they are fetched.
func (c *Crawler) discover(ctx context.Context, l Lister, cur *Cursor, pages *budget, st *crawler.RunState) error {
	if !pages.ok() {
		return nil
	}
	body, err := c.Web.Get(ctx, l.ListPath(1))
	if err != nil {
		return fmt.Errorf("listing page 1: %w", err)
	}
	pages.take()
	st.Add(func(x *crawler.Stats) { x.PagesFetched++ })
	tracks, err := l.ParseList(body)
	if err != nil {
		return fmt.Errorf("listing page 1: %w", err)
	}
	var ids []int
	for _, t := range tracks {
		if n, err := strconv.Atoi(t.ExternalID); err == nil {
			ids = append(ids, n)
			cur.ListMaxID = max(cur.ListMaxID, n)
		}
	}
	if len(ids) > 0 {
		c.DB.Where("site = ? AND id IN ?", c.Site.Name(), ids).Delete(&Probe{})
	}
	is, err := c.Ingester.Ingest(tracks, time.Now().UTC())
	st.Add(func(x *crawler.Stats) { x.SongsSeen += len(tracks); x.SongsInserted += is.Inserted })
	return err
}

// walk reads listing pages from cur.NextPage. incremental: stop at the
// first page that adds no new song. done=true at the listing's end.
func (c *Crawler) walk(ctx context.Context, l Lister, cur *Cursor, pages *budget, incremental bool, st *crawler.RunState) (done bool, err error) {
	prev := ""
	for pages.ok() {
		if ctx.Err() != nil {
			return false, nil
		}
		path := l.ListPath(cur.NextPage)
		body, err := c.Web.Get(ctx, path)
		if err != nil {
			return false, fmt.Errorf("listing page %d: %w", cur.NextPage, err)
		}
		pages.take()
		st.Add(func(x *crawler.Stats) { x.PagesFetched++ })
		tracks, err := l.ParseList(body)
		if err != nil {
			return false, fmt.Errorf("listing page %d: %w", cur.NextPage, err)
		}
		hash := scraper.Fingerprint(tracks)
		if len(tracks) == 0 || hash == prev { // past the end (empty, or a repeated last page)
			return true, nil
		}
		prev = hash
		now := time.Now().UTC()
		changed, err := crawler.PageChanged(c.DB, c.Cfg.Web.BaseURL+path, hash, now)
		if err != nil {
			return false, err
		}
		inserted := 0
		if changed {
			is, err := c.Ingester.Ingest(tracks, now)
			if err != nil {
				return false, err
			}
			inserted = is.Inserted
			st.Add(func(x *crawler.Stats) {
				x.SongsInserted += is.Inserted
				x.SongsUpdated += is.Updated
				x.SongsUnchanged += is.Unchanged
			})
		} else {
			st.Add(func(x *crawler.Stats) { x.PagesUnchanged++; x.SongsUnchanged += len(tracks) })
		}
		st.Add(func(x *crawler.Stats) { x.SongsSeen += len(tracks) })
		cur.NextPage++
		if cur.Site != "" { // a real (persisted) cursor
			if err := c.save(cur); err != nil {
				return false, err
			}
		}
		if incremental && inserted == 0 {
			return true, nil
		}
	}
	return false, nil
}

// complete returns the ids in [from,to] already stored with audio.
func (c *Crawler) complete(from, to int) (map[int]bool, error) {
	ids := make([]string, 0, to-from+1)
	for id := from; id <= to; id++ {
		ids = append(ids, strconv.Itoa(id))
	}
	var have []string
	err := c.DB.Model(&catalog.Source{}).Where("provider = ? AND external_id IN ? AND audio_url <> ''", c.Site.Name(), ids).
		Pluck("external_id", &have).Error
	out := map[int]bool{}
	for _, h := range have {
		n, _ := strconv.Atoi(h)
		out[n] = true
	}
	return out, err
}

// highestID is the highest id known: stored songs or the listing's newest.
func (c *Crawler) highestID(cur *Cursor) int {
	var top *int
	c.DB.Model(&catalog.Source{}).Where("provider = ?", c.Site.Name()).
		Select("MAX(CAST(external_id AS INTEGER))").Scan(&top)
	n := cur.ListMaxID
	if top != nil {
		n = max(n, *top)
	}
	return n
}

// sweep fetches song pages by id for ids not yet complete (stored with
// audio), up to the highest known id, then probes ahead.
func (c *Crawler) sweep(ctx context.Context, cur *Cursor, probes *budget, st *crawler.RunState) error {
	p := c.Site.(Prober)
	top := c.highestID(cur)
	for cur.SweepNextID <= top && probes.ok() && ctx.Err() == nil {
		from, to := cur.SweepNextID, min(cur.SweepNextID+199, top)
		done, err := c.complete(from, to)
		if err != nil {
			return err
		}
		// Recent misses are skipped (discover() clears the mark of any id
		// that shows up on the listing later).
		var missing []int
		c.DB.Model(&Probe{}).Where("site = ? AND id BETWEEN ? AND ? AND checked_at > ?", c.Site.Name(), from, to,
			time.Now().UTC().Add(-c.Cfg.ProbeRetryAfter)).Pluck("id", &missing)
		for _, id := range missing {
			done[id] = true
		}
		for id := from; id <= to; id++ {
			if done[id] {
				continue
			}
			if !probes.ok() || ctx.Err() != nil {
				return c.save(cur)
			}
			// Earlier probes in this chunk may have completed this id
			// (song pages embed other songs).
			if again, _ := c.complete(id, id); again[id] {
				continue
			}
			if _, err := c.probe(ctx, p, id, probes, st); err != nil {
				return err
			}
			cur.SweepNextID = id + 1
		}
		cur.SweepNextID = to + 1
		if err := c.save(cur); err != nil {
			return err
		}
	}
	if cur.SweepNextID <= top || !probes.ok() || ctx.Err() != nil {
		return c.save(cur)
	}
	misses := 0
	for id := max(cur.SweepNextID, top+1); misses < c.Cfg.ProbeAhead && probes.ok() && ctx.Err() == nil; id++ {
		found, err := c.probe(ctx, p, id, probes, st)
		if err != nil {
			return err
		}
		if found {
			misses, cur.SweepNextID = 0, id+1
		} else {
			misses++
		}
	}
	if cur.Phase == PhaseSweep && misses >= c.Cfg.ProbeAhead {
		now := time.Now().UTC()
		cur.Phase, cur.SweepDoneAt = PhaseSynced, &now
	}
	return c.save(cur)
}

func (c *Crawler) probe(ctx context.Context, p Prober, id int, probes *budget, st *crawler.RunState) (found bool, err error) {
	body, err := c.Web.Get(ctx, p.SongPath(id))
	if err != nil {
		if fatal(err) {
			return false, err
		}
		st.Fail("song %d: %v", id, err)
		return false, nil
	}
	probes.take()
	st.Add(func(x *crawler.Stats) { x.Probed++ })
	now := time.Now().UTC()
	tracks, err := p.ParseSong(body, id)
	if errors.Is(err, ErrNotFound) {
		return false, c.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&Probe{Site: c.Site.Name(), ID: id, CheckedAt: now}).Error
	}
	if err != nil {
		st.Fail("song %d: %v", id, err)
		return false, nil
	}
	is, err := c.Ingester.Ingest(tracks, now)
	if err != nil {
		return false, err
	}
	c.DB.Where("site = ? AND id = ?", c.Site.Name(), id).Delete(&Probe{})
	st.Add(func(x *crawler.Stats) {
		x.ProbeFound++
		x.SongsInserted += is.Inserted
		x.SongsUpdated += is.Updated
		x.SongsUnchanged += is.Unchanged
	})
	return true, nil
}

// ---- catalog.Searcher ----

func (c *Crawler) Name() string { return c.Site.Name() }

// Search runs the site's own search page (if it has one).
func (c *Crawler) Search(ctx context.Context, q string, limit int) ([]catalog.Track, error) {
	s, ok := c.Site.(Searchable)
	if !ok {
		return nil, catalog.ErrUnavailable
	}
	body, err := c.Web.Get(ctx, s.SearchPath(strings.TrimSpace(q)))
	if fatal(err) || errors.Is(err, webguard.ErrDisallowed) {
		return nil, fmt.Errorf("%w: %v", catalog.ErrUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	tracks, err := s.ParseSearch(body)
	if err != nil {
		return nil, err
	}
	if len(tracks) > limit {
		tracks = tracks[:limit]
	}
	return c.enrich(ctx, tracks), nil
}

// enrich fetches song pages (Prober sites) for search results that have no
// audio, up to SearchEnrich pages and while ctx allows; a page usually
// carries several songs. Only tracks with audio are returned: the catalog
// stores playable songs only.
func (c *Crawler) enrich(ctx context.Context, tracks []catalog.Track) []catalog.Track {
	p, prober := c.Site.(Prober)
	have := map[string]catalog.Track{}
	var order []string
	add := func(t catalog.Track) {
		if _, ok := have[t.ExternalID]; !ok {
			order = append(order, t.ExternalID)
		}
		if t.AudioURL != "" || have[t.ExternalID].AudioURL == "" {
			have[t.ExternalID] = t
		}
	}
	for _, t := range tracks {
		add(t)
	}
	fetched := 0
	var slowest time.Duration
	for _, t := range tracks {
		if !prober || fetched >= c.Cfg.SearchEnrich || ctx.Err() != nil {
			break
		}
		id, err := strconv.Atoi(t.ExternalID)
		if err != nil || have[t.ExternalID].AudioURL != "" {
			continue
		}
		// Stop before a fetch would overrun the caller's deadline: return
		// what we have rather than time out and lose all of it.
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < slowest+c.Cfg.Web.Delay+500*time.Millisecond {
			break
		}
		start := time.Now()
		body, err := c.Web.Get(ctx, p.SongPath(id))
		slowest = max(slowest, time.Since(start))
		fetched++
		if err != nil {
			continue
		}
		more, err := p.ParseSong(body, id)
		if err != nil {
			continue
		}
		for _, m := range more {
			add(m)
		}
	}
	out := make([]catalog.Track, 0, len(order))
	for _, id := range order {
		if have[id].AudioURL != "" {
			out = append(out, have[id])
		}
	}
	return out
}
