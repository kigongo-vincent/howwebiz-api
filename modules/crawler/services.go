package crawler

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/chart"
	"github.com/kigongo-vincent/howwebiz-api/modules/genre"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"gorm.io/gorm"
)

type CrawlerService struct {
	DB        *gorm.DB
	Engine    *Engine
	Scheduler *Scheduler
}

type crawlRequest struct {
	Targets      []string `json:"targets"`
	Pages        *int     `json:"pages"`
	FetchDetails *bool    `json:"fetchDetails"`
	Force        *bool    `json:"force"`
}

// Start triggers a manual crawl. Body is optional; omitted fields use the
// configured defaults. 202 on start, 409 if one is already running.
func (s *CrawlerService) Start(c *fiber.Ctx) error {
	opts := s.Engine.Cfg.Defaults
	if len(c.Body()) > 0 {
		var req crawlRequest
		if err := c.BodyParser(&req); err != nil {
			return base.API_ERROR(c, "invalid payload")
		}
		if len(req.Targets) > 0 {
			opts.Targets = req.Targets
		}
		if req.Pages != nil {
			opts.Pages = *req.Pages
		}
		if req.FetchDetails != nil {
			opts.FetchDetails = *req.FetchDetails
		}
		if req.Force != nil {
			opts.Force = *req.Force
		}
	}
	if err := opts.Validate(); err != nil {
		return base.API_ERROR(c, err.Error())
	}
	run, err := s.Engine.Start("manual", opts)
	if errors.Is(err, ErrBusy) {
		return c.Status(fiber.StatusConflict).JSON(base.APIResponse{Message: err.Error(), Data: fiber.Map{"runId": run.ID}})
	}
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to start crawl")
	}
	c.Location("/api/v1/crawl/runs/" + itoa(run.ID))
	return c.Status(fiber.StatusAccepted).JSON(base.APIResponse{Data: run, Message: "crawl started"})
}

func (s *CrawlerService) Status(c *fiber.Ctx) error {
	var next *time.Time
	if t := s.Scheduler.Next(); !t.IsZero() {
		next = &t
	}
	var runID *uint
	if id := s.Engine.Running(); id != 0 {
		runID = &id
	}
	return c.JSON(base.APIResponse{Data: fiber.Map{
		"running":         runID != nil,
		"runId":           runID,
		"cronEnabled":     s.Scheduler != nil,
		"nextScheduledAt": next,
		"targets":         chart.ListNames(),
		"defaults":        s.Engine.Cfg.Defaults,
	}})
}

func (s *CrawlerService) Runs(c *fiber.Ctx) error {
	p := base.ParsePage(c)
	q := s.DB.Model(&CrawlRun{})
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to count runs")
	}
	runs := []CrawlRun{}
	if err := p.Apply(q).Order("id DESC").Find(&runs).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get runs")
	}
	return c.JSON(base.APIResponse{Data: runs, Pagination: p.Controls(total, len(runs))})
}

func (s *CrawlerService) Run(c *fiber.Ctx) error {
	id, ok := base.ParamID(c, "id")
	if !ok {
		return base.API_ERROR(c, "invalid run id")
	}
	var run CrawlRun
	if err := s.DB.First(&run, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return base.API_FAIL(c, fiber.StatusNotFound, "run not found")
		}
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get run")
	}
	return c.JSON(base.APIResponse{Data: run})
}

type catalogRequest struct {
	MaxPages      *int  `json:"maxPages"`
	MaxProbes     *int  `json:"maxProbes"`
	MaxDetails    *int  `json:"maxDetails"`
	RefreshGenres *bool `json:"refreshGenres"`
	Reset         *bool `json:"reset"`
}

// StartCatalog runs the next step of the full-catalog crawl (resumable).
// Body is optional; omitted fields use CATALOG_* defaults, 0 = unlimited.
func (s *CrawlerService) StartCatalog(c *fiber.Ctx) error {
	opts := CatalogDefaultsFromEnv()
	if len(c.Body()) > 0 {
		var req catalogRequest
		if err := c.BodyParser(&req); err != nil {
			return base.API_ERROR(c, "invalid payload")
		}
		if req.MaxPages != nil {
			opts.MaxPages = *req.MaxPages
		}
		if req.MaxProbes != nil {
			opts.MaxProbes = *req.MaxProbes
		}
		if req.MaxDetails != nil {
			opts.MaxDetails = *req.MaxDetails
		}
		if req.Reset != nil {
			opts.Reset = *req.Reset
		}
		if req.RefreshGenres != nil {
			opts.RefreshGenres = *req.RefreshGenres
		}
	}
	if err := opts.Validate(); err != nil {
		return base.API_ERROR(c, err.Error())
	}
	run, err := s.Engine.StartCatalog("manual", opts)
	if errors.Is(err, ErrBusy) {
		return c.Status(fiber.StatusConflict).JSON(base.APIResponse{Message: err.Error(), Data: fiber.Map{"runId": run.ID}})
	}
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to start catalog crawl")
	}
	c.Location("/api/v1/crawl/runs/" + itoa(run.ID))
	return c.Status(fiber.StatusAccepted).JSON(base.APIResponse{Data: run, Message: "catalog crawl started"})
}

// Catalog reports how complete the local copy of howwe.ug is.
func (s *CrawlerService) Catalog(c *fiber.Ctx) error {
	cur, err := LoadCursor(s.DB)
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to load catalog cursor")
	}
	counts, err := song.Counts(s.DB, time.Now().UTC())
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to count songs")
	}
	var missing int64
	s.DB.Model(&SongProbe{}).Count(&missing)
	var siteTotal int64
	s.DB.Model(&genre.Genre{}).Select("COALESCE(SUM(site_count), 0)").Scan(&siteTotal)
	coverage := 0.0
	if siteTotal > 0 {
		coverage = float64(counts.Songs) / float64(siteTotal)
	}
	var lastRun *CrawlRun
	var r CrawlRun
	if s.DB.Where("kind = ?", KindCatalog).Order("id DESC").Limit(1).Find(&r); r.ID != 0 {
		lastRun = &r
	}
	return c.JSON(base.APIResponse{Data: fiber.Map{
		"cursor":     cur,
		"songs":      counts,
		"missingIds": missing,
		"siteTotal":  siteTotal, // sum of the song counts howwe.ug shows per genre
		"coverage":   coverage,  // songs stored / siteTotal
		"complete":   cur.Phase == PhaseSynced,
		"lastRun":    lastRun,
		"running":    s.Engine.Running() != 0,
		"defaults":   CatalogDefaultsFromEnv(),
	}})
}
