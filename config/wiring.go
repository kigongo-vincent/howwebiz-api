package config

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/audiocom"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/momuziki"
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
		Priority: base.EnvList("CATALOG_PROVIDER_PRIORITY", []string{"howwe", "ugamusic", "pearltunes", "momuziki", "audiocom"})}

	ptCfg := sitecrawl.ConfigFromEnv("PEARLTUNES", "https://pearltunes.com")
	ugCfg := sitecrawl.ConfigFromEnv("UGAMUSIC", "https://ugamusic.net")
	sites := sitecrawl.Registry{
		pearltunes.Name: sitecrawl.New(ctx, db, pearltunes.Site{Base: ptCfg.Web.BaseURL}, ptCfg, ingester),
		ugamusic.Name:   sitecrawl.New(ctx, db, ugamusic.Site{Base: ugCfg.Web.BaseURL}, ugCfg, ingester),
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
		if _, ok := sites[name].Site.(sitecrawl.Searchable); ok {
			searchers = append(searchers, sites[name])
		}
		providers = append(providers, sites[name].Site.Info())
	}

	indexer := &catalog.Indexer{DB: db, Ingester: ingester, Stores: stores, Batch: 500}
	cat := &catalog.Service{DB: db, Ingester: ingester, Indexer: indexer, Providers: providers,
		Search: &catalog.SearchService{DB: db, Ingester: ingester, Searchers: searchers,
			Timeout:  base.EnvDuration("CATALOG_LIVE_TIMEOUT", 8*time.Second),
			CacheTTL: base.EnvDuration("CATALOG_LIVE_CACHE_TTL", 15*time.Minute)}}

	deps := Dependencies{DB: db, App: app, Crawler: engine, AudioCom: audioCom, Momuziki: mom, Catalog: cat, Sites: sites}
	if background {
		// Enforce "only songs with audio" on data stored before the rule.
		if st, err := ingester.PurgeWithoutAudio(time.Now().UTC()); err != nil {
			slog.Error("catalog: purge songs without audio", "err", err)
		} else if st.SourcesRemoved > 0 || st.SongsRemoved > 0 {
			slog.Info("catalog: purged songs without audio", "stats", st)
		}
		hooks := []func(){audioCom.DailySync, mom.DailySync}
		for _, name := range sites.Names() {
			hooks = append(hooks, sites[name].DailySync)
		}
		deps.Scheduler = crawler.StartScheduler(ctx, engine, hooks...)
		indexer.Start(ctx, base.EnvDuration("CATALOG_INDEX_INTERVAL", 2*time.Minute))
	}
	return deps
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
