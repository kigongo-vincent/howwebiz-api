package sitecrawl

import (
	"crypto/subtle"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

// Registry holds every site crawler, by provider name.
type Registry map[string]*Crawler

func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for n := range r {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RouterRegister mounts the shared control endpoints:
//
//	GET  /providers/:name/status
//	POST /providers/:name/crawl
//	POST /providers/:name/guard/reset
func RouterRegister(rr *base.RouteRegister, reg Registry) {
	auth := requireAPIKey(base.Env("API_KEY", ""))
	g := rr.Router.Group("providers")
	g.Get("/:name/status", reg.with(statusHandler))
	g.Post("/:name/crawl", auth, reg.with(crawlHandler))
	g.Post("/:name/guard/reset", auth, reg.with(resetHandler))
}

func (r Registry) with(h func(*fiber.Ctx, *Crawler) error) fiber.Handler {
	return func(c *fiber.Ctx) error {
		cr, ok := r[c.Params("name")]
		if !ok {
			return base.API_FAIL(c, fiber.StatusNotFound, "no crawlable provider named "+c.Params("name"))
		}
		return h(c, cr)
	}
}

func statusHandler(c *fiber.Ctx, cr *Crawler) error {
	h, _ := cr.Web.Health()
	open, until := cr.Web.CircuitOpen()
	var sources, withAudio int64
	cr.DB.Model(&catalog.Source{}).Where("provider = ?", cr.Site.Name()).Count(&sources)
	cr.DB.Model(&catalog.Source{}).Where("provider = ? AND audio_url <> ''", cr.Site.Name()).Count(&withAudio)
	var last crawler.CrawlRun
	cr.DB.Where("kind = ?", cr.Kind()).Order("id DESC").Limit(1).Find(&last)
	var lastRun *crawler.CrawlRun
	if last.ID != 0 {
		lastRun = &last
	}
	_, lister := cr.Site.(Lister)
	_, prober := cr.Site.(Prober)
	_, searchable := cr.Site.(Searchable)
	_, sitemapper := cr.Site.(Sitemapper)
	_, apiSearch := cr.Site.(APISearcher)
	feeder, isFeeder := cr.Site.(Feeder)
	var feeds []FeedCursor
	if isFeeder {
		stored := cr.feedCursors()
		for _, name := range feeder.Feeds() {
			fc, ok := stored[name]
			if !ok {
				fc = FeedCursor{Feed: name}
			}
			feeds = append(feeds, fc)
		}
	}
	cur := cr.cursor()
	var sitemap map[string]int64
	if sitemapper {
		sitemap = cr.sitemapCounts()
	}
	return c.JSON(base.APIResponse{Data: fiber.Map{
		"provider": cr.Site.Info(), "cursor": cur, "complete": cur.Phase == PhaseSynced,
		"songs": sources, "withAudio": withAudio, "highestId": cr.highestID(&cur),
		"capabilities": fiber.Map{"listing": lister, "songPagesById": prober, "sitemap": sitemapper,
			"search": searchable || apiSearch, "feeds": isFeeder},
		"enabled": cr.enabled(), "sitemap": sitemap, "feeds": feeds,
		"running": cr.Runner.Running() != 0, "lastRun": lastRun, "defaults": cr.Cfg.Defaults,
		"guard": fiber.Map{"paused": open, "pausedUntil": until, "health": h},
	}})
}

func crawlHandler(c *fiber.Ctx, cr *Crawler) error {
	opts := cr.Cfg.Defaults
	if len(c.Body()) > 0 {
		var req struct {
			MaxPages  *int  `json:"maxPages"`
			MaxProbes *int  `json:"maxProbes"`
			Refresh   *bool `json:"refresh"`
			Reset     *bool `json:"reset"`
		}
		if err := c.BodyParser(&req); err != nil {
			return base.API_ERROR(c, "invalid payload")
		}
		if req.MaxPages != nil {
			opts.MaxPages = *req.MaxPages
		}
		if req.MaxProbes != nil {
			opts.MaxProbes = *req.MaxProbes
		}
		if req.Refresh != nil {
			opts.Refresh = *req.Refresh
		}
		if req.Reset != nil {
			opts.Reset = *req.Reset
		}
	}
	run, err := cr.Start("manual", opts)
	switch {
	case errors.Is(err, crawler.ErrBusy):
		return c.Status(fiber.StatusConflict).JSON(base.APIResponse{Message: err.Error(), Data: fiber.Map{"runId": run.ID}})
	case errors.Is(err, ErrNotConfigured):
		return base.API_FAIL(c, fiber.StatusPreconditionFailed, err.Error())
	case errors.Is(err, webguard.ErrCircuitOpen):
		return base.API_FAIL(c, fiber.StatusServiceUnavailable, err.Error())
	case err != nil:
		return base.API_ERROR(c, err.Error())
	}
	c.Location("/api/v1/crawl/runs/" + strconv.FormatUint(uint64(run.ID), 10))
	return c.Status(fiber.StatusAccepted).JSON(base.APIResponse{Data: run, Message: cr.Site.Name() + " crawl started"})
}

func resetHandler(c *fiber.Ctx, cr *Crawler) error {
	if err := cr.Web.ResetCircuit(); err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to reset")
	}
	return c.JSON(base.APIResponse{Message: cr.Site.Name() + " guard reset at " + time.Now().UTC().Format(time.RFC3339)})
}

func requireAPIKey(key string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if key != "" && subtle.ConstantTimeCompare([]byte(c.Get("X-API-Key")), []byte(key)) != 1 {
			return base.API_FAIL(c, fiber.StatusUnauthorized, "missing or invalid X-API-Key")
		}
		return c.Next()
	}
}
