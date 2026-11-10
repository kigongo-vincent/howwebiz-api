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
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/internetarchive"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/jamendo"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/pearltunes"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/ugamusic"
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
		Priority: base.EnvList("CATALOG_PROVIDER_PRIORITY", []string{"howwe", "ugamusic", "pearltunes", "momuziki", "audiocom", "audius", "jamendo", "internetarchive"})}

	ptCfg := sitecrawl.ConfigFromEnv("PEARLTUNES", "https://pearltunes.com")
	ugCfg := sitecrawl.ConfigFromEnv("UGAMUSIC", "https://ugamusic.net")
	// Full-track APIs with worldwide catalogs (see README "Sources researched").
	auCfg := sitecrawl.ConfigFromEnv("AUDIUS", "https://api.audius.co")
	jaCfg := sitecrawl.ConfigFromEnv("JAMENDO", "https://api.jamendo.com")
	iaCfg := sitecrawl.ConfigFromEnv("INTERNETARCHIVE", "https://archive.org")
	// archive.org feeds cost one /metadata request per item: a daily page budget keeps the load modest.
	iaCfg.Defaults.MaxPages = base.EnvInt("INTERNETARCHIVE_MAX_PAGES", 200)
	sites := sitecrawl.Registry{
		pearltunes.Name: sitecrawl.New(ctx, db, pearltunes.Site{Base: ptCfg.Web.BaseURL}, ptCfg, ingester),
		ugamusic.Name:   sitecrawl.New(ctx, db, ugamusic.Site{Base: ugCfg.Web.BaseURL}, ugCfg, ingester),
		audius.Name: sitecrawl.New(ctx, db, audius.Site{Base: auCfg.Web.BaseURL,
			AppName: base.Env("AUDIUS_APP_NAME", "howwebiz"), Genres: base.EnvList("AUDIUS_GENRES", audius.Genres),
			VerifiedOnly: base.EnvBool("AUDIUS_VERIFIED_ONLY", false)}, auCfg, ingester),
		jamendo.Name: sitecrawl.New(ctx, db, jamendo.Site{Base: jaCfg.Web.BaseURL, ClientID: base.Env("JAMENDO_CLIENT_ID", "")}, jaCfg, ingester),
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
		Timeout:   base.EnvDuration("CATALOG_LIVE_TIMEOUT", 8*time.Second),
		CacheTTL:  base.EnvDuration("CATALOG_LIVE_CACHE_TTL", 15*time.Minute),
		Cache:     rc,
		ResultTTL: base.EnvDuration("CATALOG_RESULT_CACHE_TTL", 60*time.Second),
		CountTTL:  base.EnvDuration("CATALOG_COUNT_CACHE_TTL", 5*time.Minute)}
	cat := &catalog.Service{DB: db, Ingester: ingester, Indexer: indexer, Providers: providers, Search: searchSvc}

	deps := Dependencies{DB: db, App: app, Crawler: engine, AudioCom: audioCom, Momuziki: mom, Catalog: cat, Sites: sites, Cache: rc}
	if !background {
		if err := searchSvc.PrepareSearchText(ctx); err != nil {
			slog.Error("catalog: search index backfill", "err", err)
		}
	}
	if background {
		// Data upkeep off the boot path: at 100k+ rows it can take a while,
		// and the API serves meanwhile (text search uses the per-column match
		// until search_text is filled).
		go func() {
			_ = base.Safe("catalog upkeep", func() {
				// Enforce "only songs with audio" on data stored before the rule.
				if st, err := ingester.PurgeWithoutAudio(time.Now().UTC()); err != nil {
					slog.Error("catalog: purge songs without audio", "err", err)
				} else if st.SourcesRemoved > 0 || st.SongsRemoved > 0 {
					slog.Info("catalog: purged songs without audio", "stats", st)
					searchSvc.Bump(ctx)
				}
				if err := searchSvc.PrepareSearchText(ctx); err != nil {
					slog.Error("catalog: search index backfill", "err", err)
				}
			})
		}()
		hooks := []func(){audioCom.DailySync, mom.DailySync}
		for _, name := range sites.Names() {
			hooks = append(hooks, sites[name].DailySync)
		}
		deps.Scheduler = crawler.StartScheduler(ctx, engine, hooks...)
		indexer.Start(ctx, base.EnvDuration("CATALOG_INDEX_INTERVAL", 2*time.Minute))
	}
	return deps
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

// Wait blocks until every crawler has stopped (after ctx is cancelled).
func (d Dependencies) Wait() {
	d.Crawler.Wait()
	d.AudioCom.Runner.Wait()
	d.Momuziki.Runner.Wait()
	for _, s := range d.Sites {
		s.Runner.Wait()
	}
}
