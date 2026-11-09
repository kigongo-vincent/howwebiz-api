package momuziki

import (
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
)

// DailySync is called by the daily scheduler.
func (s *Service) DailySync() {
	if !s.Cfg.Daily {
		return
	}
	if open, until := s.Web.CircuitOpen(); open {
		slog.Info("momuziki: skipping scheduled crawl, paused after blocks", "until", until)
		return
	}
	if _, err := s.StartCrawl("cron", s.Cfg.Defaults); err != nil {
		slog.Warn("momuziki: scheduled crawl not started", "err", err)
	}
}

func (s *Service) Status(c *fiber.Ctx) error {
	h, _ := s.Web.Health()
	open, until := s.Web.CircuitOpen()
	var songs, withAudio, due, cats int64
	s.DB.Model(&Song{}).Count(&songs)
	s.DB.Model(&Song{}).Where("audio_url <> ''").Count(&withAudio)
	s.DB.Model(&Song{}).Where("next_detail_check_at IS NULL OR next_detail_check_at <= ?", time.Now().UTC()).Count(&due)
	s.DB.Model(&Category{}).Count(&cats)
	var last crawler.CrawlRun
	s.DB.Where("kind = ?", Kind).Order("id DESC").Limit(1).Find(&last)
	var lastRun *crawler.CrawlRun
	if last.ID != 0 {
		lastRun = &last
	}
	return c.JSON(base.APIResponse{Data: fiber.Map{
		"cursor": s.loadCursor(), "complete": s.loadCursor().Phase == PhaseSynced,
		"songs": songs, "withAudio": withAudio, "detailsDue": due, "categories": cats, "highestId": maxID(s.DB),
		"running": s.Runner.Running() != 0, "lastRun": lastRun, "defaults": s.Cfg.Defaults,
		"guard": fiber.Map{"paused": open, "pausedUntil": until, "health": h},
	}})
}

func (s *Service) Crawl(c *fiber.Ctx) error {
	opts := s.Cfg.Defaults
	if len(c.Body()) > 0 {
		var req struct {
			MaxPages          *int  `json:"maxPages"`
			MaxProbes         *int  `json:"maxProbes"`
			MaxDetails        *int  `json:"maxDetails"`
			RefreshCategories *bool `json:"refreshCategories"`
			Reset             *bool `json:"reset"`
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
		if req.MaxDetails != nil {
			opts.MaxDetails = *req.MaxDetails
		}
		if req.RefreshCategories != nil {
			opts.RefreshCategories = *req.RefreshCategories
		}
		if req.Reset != nil {
			opts.Reset = *req.Reset
		}
	}
	run, err := s.StartCrawl("manual", opts)
	switch {
	case errors.Is(err, crawler.ErrBusy):
		return c.Status(fiber.StatusConflict).JSON(base.APIResponse{Message: err.Error(), Data: fiber.Map{"runId": run.ID}})
	case errors.Is(err, webguard.ErrCircuitOpen):
		return base.API_FAIL(c, fiber.StatusServiceUnavailable, err.Error())
	case err != nil:
		return base.API_ERROR(c, err.Error())
	}
	c.Location("/api/v1/crawl/runs/" + strconv.FormatUint(uint64(run.ID), 10))
	return c.Status(fiber.StatusAccepted).JSON(base.APIResponse{Data: run, Message: "momuziki crawl started"})
}

func (s *Service) ResetGuard(c *fiber.Ctx) error {
	if err := s.Web.ResetCircuit(); err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to reset")
	}
	return c.JSON(base.APIResponse{Message: "momuziki guard reset; blocks are still counted"})
}

// Search runs momuziki's own search (search.php?query=…) and stores the
// results. If the site is blocking us (guard paused/tripped) it falls back
// to the local catalog and says so in meta.
//
//	?q=  ?source=live (default) | local
func (s *Service) Search(c *fiber.Ctx) error {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		return base.API_ERROR(c, "q is required")
	}
	if src := c.Query("source", "live"); src == "local" {
		return s.local(c, q, "")
	} else if src != "live" {
		return base.API_ERROR(c, "source must be live or local")
	}
	body, err := s.Web.Get(c.UserContext(), "/search.php?"+url.Values{"query": {q}}.Encode())
	if fatal(err) {
		return s.local(c, q, err.Error())
	}
	if err != nil {
		return base.API_FAIL(c, fiber.StatusBadGateway, err.Error())
	}
	rows, err := ParseRows(body, s.base)
	if err != nil {
		return base.API_FAIL(c, fiber.StatusBadGateway, err.Error())
	}
	if _, err := UpsertRows(s.DB, s.Cfg.Web.BaseURL, rows, 0, time.Now().UTC()); err != nil {
		slog.Warn("momuziki: store search results", "err", err)
	}
	ids := make([]uint, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	var stored []Song
	s.DB.Where("id IN ?", ids).Find(&stored)
	byID := map[uint]Song{}
	for _, x := range stored {
		byID[x.ID] = x
	}
	out := make([]Song, 0, len(rows)) // site's relevance order, enriched from the DB
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return c.JSON(base.APIResponse{Data: out,
		Pagination: &base.PaginationControls{Page: 1, Limit: uint(len(out)), Total: uint(len(out)), TotalPages: 1},
		Meta:       fiber.Map{"source": "momuziki.com", "query": q}})
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Service) local(c *fiber.Ctx, q, fallbackReason string) error {
	p := base.ParsePage(c)
	db := s.DB.Model(&Song{})
	for _, w := range strings.Fields(strings.ToLower(q)) {
		like := "%" + likeEscape(w) + "%"
		db = db.Where(`(LOWER(title) LIKE ? ESCAPE '\' OR LOWER(artist) LIKE ? ESCAPE '\')`, like, like)
	}
	var total int64
	db.Count(&total)
	songs := []Song{}
	if err := p.Apply(db).Order("plays IS NULL, plays DESC, id DESC").Find(&songs).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "search failed")
	}
	meta := fiber.Map{"source": "local", "query": q}
	if fallbackReason != "" {
		meta["fallback"] = "momuziki.com unavailable: " + fallbackReason
	}
	return c.JSON(base.APIResponse{Data: songs, Pagination: p.Controls(total, len(songs)), Meta: meta})
}

var sorts = map[string]string{
	"recent": "id DESC",
	"title":  "LOWER(title), id",
	"plays":  "plays IS NULL, plays DESC, id DESC",
}

// Songs lists stored songs: ?q= ?categoryId= ?artist= ?hasAudio= ?sort=recent|title|plays
func (s *Service) Songs(c *fiber.Ctx) error {
	if q := strings.TrimSpace(c.Query("q")); q != "" && c.Query("categoryId") == "" && c.Query("artist") == "" {
		return s.local(c, q, "")
	}
	p := base.ParsePage(c)
	db := s.DB.Model(&Song{})
	if v := c.Query("categoryId", c.Params("id")); v != "" {
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return base.API_ERROR(c, "categoryId must be a number")
		}
		db = db.Where("category_id = ?", id)
	}
	if v := c.Query("artist"); v != "" {
		db = db.Where("LOWER(artist) = ?", strings.ToLower(v))
	}
	if v := c.Query("q"); v != "" {
		like := "%" + likeEscape(strings.ToLower(v)) + "%"
		db = db.Where(`(LOWER(title) LIKE ? ESCAPE '\' OR LOWER(artist) LIKE ? ESCAPE '\')`, like, like)
	}
	if v := c.Query("hasAudio", "true"); v != "" {
		has, err := strconv.ParseBool(v)
		if err != nil {
			return base.API_ERROR(c, "hasAudio must be true or false")
		}
		if has {
			db = db.Where("audio_url <> ''")
		} else {
			db = db.Where("audio_url = ''")
		}
	}
	order, ok := sorts[c.Query("sort", "recent")]
	if !ok {
		return base.API_ERROR(c, "sort must be one of: recent, title, plays")
	}
	var total int64
	db.Count(&total)
	songs := []Song{}
	if err := p.Apply(db).Order(order).Find(&songs).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get songs")
	}
	return c.JSON(base.APIResponse{Data: songs, Pagination: p.Controls(total, len(songs))})
}

func (s *Service) Song(c *fiber.Ctx) error {
	id, ok := base.ParamID(c, "id")
	if !ok {
		return base.API_ERROR(c, "invalid song id")
	}
	var song Song
	if s.DB.Where("id = ?", id).Limit(1).Find(&song); song.ID == 0 {
		return base.API_FAIL(c, fiber.StatusNotFound, "song not found")
	}
	return c.JSON(base.APIResponse{Data: song})
}

func (s *Service) Categories(c *fiber.Ctx) error {
	cats := []Category{}
	s.DB.Order("id").Find(&cats)
	var counts []struct {
		CategoryID uint
		N          int64
	}
	s.DB.Model(&Song{}).Select("category_id, COUNT(*) AS n").Group("category_id").Scan(&counts)
	byID := map[uint]int64{}
	for _, r := range counts {
		byID[r.CategoryID] = r.N
	}
	for i := range cats {
		cats[i].Songs = byID[cats[i].ID]
	}
	return c.JSON(base.APIResponse{Data: cats, Meta: fiber.Map{"uncategorized": byID[0]}})
}
