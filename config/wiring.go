package config

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/audiocom"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/cache"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/momuziki"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/audius"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/busoga"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/fma"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/hearthis"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/incompetech"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/internetarchive"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/mdundo"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/mzikii"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/ncs"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/pearltunes"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/ugamusic"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/ugmuziki"
	"github.com/kigongo-vincent/howwebiz-api/modules/search"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
)

// Wire is the composition root: it builds every service and plugs the
// providers into the catalog. With background=true it also starts the
// daily scheduler and the catalog indexer (the server); tests pass false.
func Wire(ctx context.Context, db *gorm.DB, app *fiber.App, background bool) Dependencies {
	engine := crawler.NewEngine(ctx, db, crawler.ConfigFromEnv())
	audioCom := audiocom.NewService(ctx, db, audiocom.ConfigFromEnv(), audiocom.WebConfigFromEnv())
	mom := momuziki.NewService(ctx, db, momuziki.ConfigFromEnv())

	ingester := &catalog.Ingester{DB: db,
		Priority: base.EnvList("CATALOG_PROVIDER_PRIORITY", []string{"howwe", "ugamusic", "pearltunes", "mdundo", "momuziki",
			"mzikii", "ugmuziki", "busoga", "audiocom", "audius", "fma", "ncs", "hearthis", "incompetech", "internetarchive"})}

	ptCfg := sitecrawl.ConfigFromEnv("PEARLTUNES", "https://pearltunes.com")
	// More Ugandan / African sources (see each package's doc for what it reads).
	mdCfg := sitecrawl.ConfigFromEnv("MDUNDO", "https://play.mdundo.com")
	mzCfg := sitecrawl.ConfigFromEnv("MZIKII", "https://www.mzikii.com")
	umCfg := sitecrawl.ConfigFromEnv("UGMUZIKI", "https://www.ugmuziki.com")
	bsCfg := sitecrawl.ConfigFromEnv("BUSOGA", "https://www.busogamusicstore.com")
	// Free-licensed catalogs across every genre (hip hop, R&B, soul, jazz, ...).
	fmCfg := sitecrawl.ConfigFromEnv("FMA", "https://freemusicarchive.org")
	htCfg := sitecrawl.ConfigFromEnv("HEARTHIS", "https://api-v2.hearthis.at")
	ncCfg := sitecrawl.ConfigFromEnv("NCS", "https://ncs.io")
	icCfg := sitecrawl.ConfigFromEnv("INCOMPETECH", "https://incompetech.com")
	ugCfg := sitecrawl.ConfigFromEnv("UGAMUSIC", "https://ugamusic.net")
	// Full-track APIs with worldwide catalogs (see README "Sources researched").
	auCfg := sitecrawl.ConfigFromEnv("AUDIUS", "https://api.audius.co")
	iaCfg := sitecrawl.ConfigFromEnv("INTERNETARCHIVE", "https://archive.org")
	// archive.org feeds cost one /metadata request per item: a daily page budget keeps the load modest.
	iaCfg.Defaults.MaxPages = base.EnvInt("INTERNETARCHIVE_MAX_PAGES", 200)
	sites := sitecrawl.Registry{
		pearltunes.Name: sitecrawl.New(ctx, db, pearltunes.Site{Base: ptCfg.Web.BaseURL}, ptCfg, ingester),
		ugamusic.Name:   sitecrawl.New(ctx, db, ugamusic.Site{Base: ugCfg.Web.BaseURL}, ugCfg, ingester),
		mdundo.Name: sitecrawl.New(ctx, db, mdundo.Site{Base: mdCfg.Web.BaseURL,
			Feed: base.EnvList("MDUNDO_FEEDS", mdundo.DefaultFeeds)}, mdCfg, ingester),
		mzikii.Name:   sitecrawl.New(ctx, db, mzikii.Site{Base: mzCfg.Web.BaseURL}, mzCfg, ingester),
		ugmuziki.Name: sitecrawl.New(ctx, db, ugmuziki.Site{Base: umCfg.Web.BaseURL}, umCfg, ingester),
		fma.Name: sitecrawl.New(ctx, db, fma.Site{Base: fmCfg.Web.BaseURL,
			Genres: base.EnvList("FMA_GENRES", fma.DefaultGenres)}, fmCfg, ingester),
		hearthis.Name: sitecrawl.New(ctx, db, hearthis.Site{Base: htCfg.Web.BaseURL,
			Genres: base.EnvList("HEARTHIS_GENRES", hearthis.DefaultGenres)}, htCfg, ingester),
		ncs.Name:         sitecrawl.New(ctx, db, ncs.Site{Base: ncCfg.Web.BaseURL}, ncCfg, ingester),
		incompetech.Name: sitecrawl.New(ctx, db, incompetech.Site{Base: icCfg.Web.BaseURL}, icCfg, ingester),
		busoga.Name: sitecrawl.New(ctx, db, busoga.Site{Base: bsCfg.Web.BaseURL,
			MaxSongs: base.EnvInt("BUSOGA_MAX_SONGS", busoga.MaxSongsPerFeed)}, bsCfg, ingester),
		audius.Name: sitecrawl.New(ctx, db, audius.Site{Base: auCfg.Web.BaseURL,
			AppName: base.Env("AUDIUS_APP_NAME", "howwebiz"), Genres: base.EnvList("AUDIUS_GENRES", audius.Genres),
			Charts:       base.EnvList("AUDIUS_CHARTS", audius.Charts),
			VerifiedOnly: base.EnvBool("AUDIUS_VERIFIED_ONLY", false)}, auCfg, ingester),
		internetarchive.Name: sitecrawl.New(ctx, db, internetarchive.Site{Base: iaCfg.Web.BaseURL,
			Collections: base.EnvList("INTERNETARCHIVE_COLLECTIONS", internetarchive.DefaultCollections),
			Items:       base.EnvInt("INTERNETARCHIVE_ITEMS_PER_PAGE", 25)}, iaCfg, ingester),
	}

	// Ports: providers with their own store are indexed; every provider
	// with a search page is asked live by /songs.
	stores := []catalog.Store{song.CatalogStore{DB: db}, momuziki.CatalogStore{S: mom}, audiocom.CatalogStore{S: audioCom}}
	searchers := []catalog.Searcher{search.CatalogSearcher{Howwe: search.HowweFromEnv(), DB: db}, momuziki.CatalogSearcher{S: mom},
		audiocom.CatalogSearcher{S: audioCom}}
	providers := []catalog.ProviderInfo{
		{Name: "howwe", Site: "https://www.howwe.ug", Ingest: "index", LiveSearch: true,
			Description: "howwe.ug — Hot 100, /audio lists and the full catalog via /genres (live search needs GOOGLE_CSE_KEY)"},
		{Name: "momuziki", Site: "https://www.momuziki.com", Ingest: "index", LiveSearch: true,
			Description: "momuziki.com — categories, song pages, search (HTML pages)"},
		{Name: "audiocom", Site: "https://audio.com", Ingest: "index", LiveSearch: true,
			Description: "audio.com — official API once connected, website pages behind the block counter"},
	}
	for _, name := range sites.Names() {
		_, html := sites[name].Site.(sitecrawl.Searchable)
		_, api := sites[name].Site.(sitecrawl.APISearcher)
		if html || api {
			searchers = append(searchers, sites[name])
		}
		providers = append(providers, sites[name].Site.Info())
	}

	rc := newCache(ctx)
	indexer := &catalog.Indexer{DB: db, Ingester: ingester, Stores: stores, Batch: 500}
	searchSvc := &catalog.SearchService{DB: db, Ingester: ingester, Searchers: searchers,
		Timeout:       base.EnvDuration("CATALOG_LIVE_TIMEOUT", 8*time.Second),
		CacheTTL:      base.EnvDuration("CATALOG_LIVE_CACHE_TTL", 15*time.Minute),
		Cache:         rc,
		ResultTTL:     base.EnvDuration("CATALOG_RESULT_CACHE_TTL", 60*time.Second),
		CountTTL:      base.EnvDuration("CATALOG_COUNT_CACHE_TTL", 5*time.Minute),
		BackfillPause: base.EnvDuration("CATALOG_BACKFILL_PAUSE", 100*time.Millisecond)}
	searchSvc.Fuzzy = trigramReady(db)
	cat := &catalog.Service{DB: db, Ingester: ingester, Indexer: indexer, Providers: providers, Search: searchSvc}

	deps := Dependencies{DB: db, App: app, Crawler: engine, AudioCom: audioCom, Momuziki: mom, Catalog: cat, Sites: sites, Cache: rc}
	// Database upkeep: indexes, the audio-only purge, the search_text
	// backfill, then its index. None of it is on the boot path: on a large
	// catalog or a slow database it takes minutes, and the API serves
	// meanwhile (just slower until the indexes exist).
	dialect := db.Dialector.Name()
	upkeep := func(purge bool) {
		start := time.Now()
		base.EnsureIndexes(ctx, db, appIndexes(dialect))
		if purge {
			// Enforce "only songs with audio" on data stored before the rule.
			if st, err := ingester.PurgeWithoutAudio(time.Now().UTC()); err != nil {
				slog.Error("catalog: purge songs without audio", "err", err)
			} else if st.SourcesRemoved > 0 || st.SongsRemoved > 0 {
				slog.Info("catalog: purged songs without audio", "stats", st)
				searchSvc.Bump(ctx)
			}
		}
		if err := searchSvc.PrepareSearchText(ctx); err != nil {
			slog.Error("catalog: search index backfill", "err", err)
		}
		base.EnsureIndexes(ctx, db, catalog.SearchIndexes(dialect))
		if took := time.Since(start); took > time.Second {
			slog.Info("database upkeep done", "took", took.Round(time.Second))
		}
	}
	if !background {
		upkeep(false) // tests: synchronous, nothing to purge
	}
	if background {
		go func() { _ = base.Safe("database upkeep", func() { upkeep(true) }) }()
		hooks := []func(){audioCom.DailySync, mom.DailySync}
		for _, name := range sites.Names() {
			hooks = append(hooks, sites[name].DailySync)
		}
		// Safe restart: runs a shutdown or crash stopped continue first;
		// the startup round then skips sources busy resuming.
		deps.ResumeInterrupted()
		deps.Scheduler = crawler.StartScheduler(ctx, engine, hooks...)
		indexer.Start(ctx, base.EnvDuration("CATALOG_INDEX_INTERVAL", 2*time.Minute))
	}
	return deps
}

// trigramReady reports whether typo-tolerant search can run (Postgres
// with pg_trgm, which search.Setup enables).
func trigramReady(db *gorm.DB) bool {
	if db.Dialector.Name() != "postgres" {
		return false
	}
	var n int64
	db.Raw(`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_trgm'`).Scan(&n)
	if n == 0 {
		slog.Warn("catalog: pg_trgm missing, typo-tolerant search off")
	}
	return n > 0
}

// newCache connects to Redis (REDIS_ADDR, default the in-container
// 127.0.0.1:6379). The API runs uncached when Redis is off or down.
func newCache(ctx context.Context) *cache.Cache {
	if !base.EnvBool("REDIS_ENABLED", true) {
		slog.Info("cache: disabled (REDIS_ENABLED=false)")
		return nil
	}
	addr := base.Env("REDIS_ADDR", cache.DefaultAddr)
	c := cache.New(addr, "howwebiz:")
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := c.Ping(pctx); err != nil {
		slog.Warn("cache: redis not reachable yet, serving uncached until it is", "addr", addr, "err", err)
	} else {
		slog.Info("cache: redis connected", "addr", addr)
	}
	return c
}

// Runners: every source's crawl runner.
func (d Dependencies) Runners() []*crawler.Runner {
	out := []*crawler.Runner{d.Crawler.Runner, d.AudioCom.Runner, d.Momuziki.Runner}
	for _, name := range d.Sites.Names() {
		out = append(out, d.Sites[name].Runner)
	}
	return out
}

// ResumeInterrupted continues each source's run that a shutdown or crash
// stopped (see crawler.Runner.ResumeInterrupted).
func (d Dependencies) ResumeInterrupted() {
	for _, r := range d.Runners() {
		if _, err := r.ResumeInterrupted(); err != nil {
			slog.Error("crawler: resume interrupted run", "err", err)
		}
	}
}

// Wait blocks until every crawler has stopped (after ctx is cancelled).
func (d Dependencies) Wait() {
	for _, r := range d.Runners() {
		r.Wait()
	}
}

// WaitFor waits up to grace for every crawler to stop at a safe point.
// Runs still going after that are recorded as interrupted (their progress
// is already saved; they resume on the next start). Reports whether all
// stopped in time.
func (d Dependencies) WaitFor(grace time.Duration) bool {
	done := make(chan struct{})
	go func() { d.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		for _, r := range d.Runners() {
			r.MarkInterrupted()
		}
		return false
	}
}
