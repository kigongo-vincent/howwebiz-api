package audiocom

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"gorm.io/gorm"
)

type Config struct {
	APIBase     string
	RedirectURI string
	RatePerMin  int
	Tags        []string
	TagRefresh  time.Duration
	SearchTTL   time.Duration
	Daily       bool
}

func ConfigFromEnv() Config {
	return Config{
		APIBase:     strings.TrimRight(base.Env("AUDIOCOM_API_BASE", "https://api.audio.com"), "/"),
		RedirectURI: base.Env("AUDIOCOM_REDIRECT_URL", "http://localhost:"+base.Env("PORT", "3000")+"/api/v1/audiocom/callback"),
		RatePerMin:  max(1, base.EnvInt("AUDIOCOM_RATE_PER_MIN", 55)), // API limit is 60
		Tags:        base.EnvList("AUDIOCOM_TAGS", nil),
		TagRefresh:  base.EnvDuration("AUDIOCOM_TAG_REFRESH", 7*24*time.Hour),
		SearchTTL:   base.EnvDuration("AUDIOCOM_SEARCH_CACHE_TTL", 10*time.Minute),
		Daily:       base.EnvBool("AUDIOCOM_DAILY", true),
	}
}

// Service is the audio.com source: OAuth connection, API client, its own
// crawl Runner (independent of howwe.ug crawls) and HTTP handlers.
type Service struct {
	DB     *gorm.DB
	Cfg    Config
	OAuth  *OAuth
	Client *Client
	Web    *Web
	Runner *crawler.Runner
	log    *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedSearch
}

type cachedSearch struct {
	res     []SearchResult
	expires time.Time
}

func NewService(ctx context.Context, db *gorm.DB, cfg Config, web WebConfig) *Service {
	hc := &http.Client{Timeout: 20 * time.Second}
	o := &OAuth{DB: db, APIBase: cfg.APIBase, RedirectURI: cfg.RedirectURI, http: hc}
	log := slog.Default().With("module", "audiocom")
	s := &Service{
		DB: db, Cfg: cfg, OAuth: o, log: log,
		Client: &Client{APIBase: cfg.APIBase, OAuth: o, http: hc, gap: time.Minute / time.Duration(cfg.RatePerMin)},
		Web:    NewWeb(db, web),
		// One runner for API and website crawls: never two crawls of
		// audio.com at once.
		Runner: crawler.NewRunner(ctx, db, log, KindAudiocom, KindAudiocomWeb),
		cache:  map[string]cachedSearch{},
	}
	s.Runner.OnResume(KindAudiocom, func(trigger string, raw json.RawMessage) (*crawler.CrawlRun, error) {
		var o CrawlOptions
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		return s.StartCrawl(trigger, o)
	})
	s.Runner.OnResume(KindAudiocomWeb, func(trigger string, raw json.RawMessage) (*crawler.CrawlRun, error) {
		var o WebCrawlOptions
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		return s.StartWebCrawl(trigger, o)
	})
	return s
}

// DailySync is called by the daily scheduler: the API crawl when
// connected, then the website crawl unless its circuit is open.
func (s *Service) DailySync() {
	if s.Cfg.Daily && s.OAuth.Connected() {
		if _, err := s.StartCrawl("cron", CrawlOptions{MaxPages: base.EnvInt("AUDIOCOM_DAILY_MAX_PAGES", 0)}); err != nil {
			s.log.Warn("audio.com: scheduled crawl not started", "err", err)
		}
		s.Runner.Wait()
	}
	if s.Web.Cfg.Daily {
		if open, until := s.Web.CircuitOpen(); open {
			s.log.Info("audio.com web: skipping scheduled crawl, paused after blocks", "until", until)
			return
		}
		if _, err := s.StartWebCrawl("cron", WebCrawlOptions{MaxPages: base.EnvInt("AUDIOCOM_WEB_DAILY_MAX_PAGES", 500)}); err != nil {
			s.log.Warn("audio.com web: scheduled crawl not started", "err", err)
		}
	}
}

// ---- website handlers ----

func (s *Service) WebStatus(c *fiber.Ctx) error {
	h, err := s.Web.Health()
	if err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to load web health")
	}
	open, until := s.Web.CircuitOpen()
	feeds := []Feed{}
	s.DB.Where("name LIKE ?", webFeedPrefix+"%").Order("name").Find(&feeds)
	var last crawler.CrawlRun
	s.DB.Where("kind = ?", KindAudiocomWeb).Order("id DESC").Limit(1).Find(&last)
	var lastRun *crawler.CrawlRun
	if last.ID != 0 {
		lastRun = &last
	}
	cfg := s.Web.Cfg
	return c.JSON(base.APIResponse{Data: fiber.Map{
		"paused":      open,
		"pausedUntil": until,
		"health":      h,
		"feeds":       feeds,
		"lastRun":     lastRun,
		"running":     s.Runner.Running() != 0,
		"guard": fiber.Map{
			"maxConsecutiveBlocks": cfg.Config.MaxConsecutive, "maxBlocksPerRun": cfg.MaxPerRun,
			"backoff": cfg.Backoff.String(), "cooldown": cfg.Cooldown.String(), "maxCooldown": cfg.MaxCooldown.String(),
			"maxUnrecognizedPages": cfg.MaxUnrecognized, "delay": cfg.Delay.String(), "userAgent": cfg.UserAgent,
		},
	}})
}

func (s *Service) WebReset(c *fiber.Ctx) error {
	if err := s.Web.ResetCircuit(); err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to reset")
	}
	return c.JSON(base.APIResponse{Message: "web circuit reset; the next request goes out (blocks are still counted)"})
}

func (s *Service) WebCrawl(c *fiber.Ctx) error {
	var opts WebCrawlOptions
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&opts); err != nil {
			return base.API_ERROR(c, "invalid payload")
		}
	}
	run, err := s.StartWebCrawl("manual", opts)
	switch {
	case errors.Is(err, crawler.ErrBusy):
		return c.Status(fiber.StatusConflict).JSON(base.APIResponse{Message: err.Error(), Data: fiber.Map{"runId": run.ID}})
	case errors.Is(err, ErrCircuitOpen):
		return base.API_FAIL(c, fiber.StatusServiceUnavailable, err.Error())
	case err != nil:
		return base.API_ERROR(c, err.Error())
	}
	c.Location("/api/v1/crawl/runs/" + strconv.FormatUint(uint64(run.ID), 10))
	return c.Status(fiber.StatusAccepted).JSON(base.APIResponse{Data: run, Message: "audio.com website crawl started"})
}

// WebSearchHandler scrapes audio.com/search?search_text=…&sort=relevance.
func (s *Service) WebSearchHandler(c *fiber.Ctx) error {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		return base.API_ERROR(c, "q is required")
	}
	page := base.ParsePage(c).Page
	pd, err := s.WebSearch(c.UserContext(), q, page)
	switch {
	case errors.Is(err, ErrCircuitOpen), errors.Is(err, ErrBlocked):
		return base.API_FAIL(c, fiber.StatusServiceUnavailable, err.Error()+" — try /api/v1/audiocom/search (official API)")
	case errors.Is(err, ErrDisallowed):
		return base.API_FAIL(c, fiber.StatusForbidden, err.Error())
	case err != nil:
		return base.API_FAIL(c, fiber.StatusBadGateway, err.Error())
	}
	tracks := make([]Track, 0, len(pd.Tracks))
	for _, a := range pd.Tracks {
		tracks = append(tracks, toTrack(a))
	}
	return c.JSON(base.APIResponse{
		Data:       tracks,
		Pagination: &base.PaginationControls{Page: uint(page), Limit: uint(len(tracks)), HasMore: len(tracks) > 0},
		Meta:       fiber.Map{"source": "audio.com website", "query": q, "embedded": pd.Sources},
	})
}

// ---- handlers ----

// Connect starts the OAuth flow. Open it in a browser (?redirect=true
// sends you straight to audio.com's sign-in), or take authorizeUrl.
func (s *Service) Connect(c *fiber.Ctx) error {
	u, err := s.OAuth.AuthorizeURL(c.UserContext())
	if err != nil {
		return base.API_FAIL(c, fiber.StatusBadGateway, err.Error())
	}
	if c.QueryBool("redirect") {
		return c.Redirect(u, fiber.StatusFound)
	}
	return c.JSON(base.APIResponse{Data: fiber.Map{"authorizeUrl": u, "redirectUri": s.Cfg.RedirectURI},
		Message: "open authorizeUrl in a browser and sign in to audio.com"})
}

func (s *Service) Callback(c *fiber.Ctx) error {
	if e := c.Query("error"); e != "" {
		return base.API_ERROR(c, "audio.com authorization failed: "+e+" "+c.Query("error_description"))
	}
	if err := s.OAuth.Callback(c.UserContext(), c.Query("code"), c.Query("state")); err != nil {
		return base.API_ERROR(c, err.Error())
	}
	return c.JSON(base.APIResponse{Message: "audio.com connected; you can close this tab"})
}

func (s *Service) Disconnect(c *fiber.Ctx) error {
	if err := s.OAuth.Disconnect(); err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to disconnect")
	}
	return c.JSON(base.APIResponse{Message: "audio.com disconnected"})
}

func (s *Service) Status(c *fiber.Ctx) error {
	a, _ := s.OAuth.load()
	feeds := []Feed{}
	s.DB.Order("name").Find(&feeds)
	var tracks, withAudio int64
	s.DB.Model(&Track{}).Count(&tracks)
	s.DB.Model(&Track{}).Where("play_url <> ''").Count(&withAudio)
	var last crawler.CrawlRun
	s.DB.Where("kind = ?", KindAudiocom).Order("id DESC").Limit(1).Find(&last)
	var lastRun *crawler.CrawlRun
	if last.ID != 0 {
		lastRun = &last
	}
	return c.JSON(base.APIResponse{Data: fiber.Map{
		"connected":   s.OAuth.Connected(),
		"connectedAt": a.ConnectedAt,
		"tracks":      tracks,
		"withAudio":   withAudio,
		"feeds":       feeds,
		"running":     s.Runner.Running() != 0,
		"lastRun":     lastRun,
		"ratePerMin":  s.Cfg.RatePerMin,
		"tags":        s.Cfg.Tags,
	}})
}

type crawlRequest struct {
	Tags     []string `json:"tags"`
	SkipAll  *bool    `json:"skipAll"`
	MaxPages *int     `json:"maxPages"`
	Reset    *bool    `json:"reset"`
}

func (s *Service) Crawl(c *fiber.Ctx) error {
	var opts CrawlOptions
	if len(c.Body()) > 0 {
		var req crawlRequest
		if err := c.BodyParser(&req); err != nil {
			return base.API_ERROR(c, "invalid payload")
		}
		opts.Tags = req.Tags
		if req.SkipAll != nil {
			opts.SkipAll = *req.SkipAll
		}
		if req.MaxPages != nil {
			opts.MaxPages = *req.MaxPages
		}
		if req.Reset != nil {
			opts.Reset = *req.Reset
		}
	}
	run, err := s.StartCrawl("manual", opts)
	switch {
	case errors.Is(err, crawler.ErrBusy):
		return c.Status(fiber.StatusConflict).JSON(base.APIResponse{Message: err.Error(), Data: fiber.Map{"runId": run.ID}})
	case errors.Is(err, ErrNotConnected):
		return base.API_FAIL(c, fiber.StatusPreconditionFailed, err.Error())
	case err != nil:
		return base.API_ERROR(c, err.Error())
	}
	c.Location("/api/v1/crawl/runs/" + strconv.FormatUint(uint64(run.ID), 10))
	return c.Status(fiber.StatusAccepted).JSON(base.APIResponse{Data: run, Message: "audio.com crawl started"})
}

// Search proxies audio.com's official search (same results as
// audio.com/search?search_text=…, relevance-ranked, 10 per page).
// Audio hits are stored, so searching also grows the local catalog.
//
//	?q=      text, or "#tag" for an exact tag
//	?types=  audio,author,collection (default all)
//	?page=
func (s *Service) Search(c *fiber.Ctx) error {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		return base.API_ERROR(c, "q is required")
	}
	types := c.Query("types")
	for _, t := range strings.Split(types, ",") {
		if t != "" && t != "audio" && t != "author" && t != "collection" {
			return base.API_ERROR(c, "types must be a comma-separated subset of: audio, author, collection")
		}
	}
	page := base.ParsePage(c).Page
	key := strings.ToLower(q) + "|" + types + "|" + strconv.Itoa(page)

	s.mu.Lock()
	hit, ok := s.cache[key]
	s.mu.Unlock()
	cached := ok && time.Now().Before(hit.expires)
	res := hit.res
	if !cached {
		var err error
		if res, err = s.Client.Search(c.UserContext(), q, types, page); err != nil {
			if errors.Is(err, ErrNotConnected) {
				return base.API_FAIL(c, fiber.StatusPreconditionFailed, err.Error())
			}
			return base.API_FAIL(c, fiber.StatusBadGateway, err.Error())
		}
		if res == nil {
			res = []SearchResult{}
		}
		s.mu.Lock()
		if len(s.cache) > 2000 {
			s.cache = map[string]cachedSearch{}
		}
		s.cache[key] = cachedSearch{res: res, expires: time.Now().Add(s.Cfg.SearchTTL)}
		s.mu.Unlock()
		if _, err := Upsert(s.DB, audioResults(res), time.Now().UTC()); err != nil {
			s.log.Warn("audio.com: store search results", "err", err)
		}
	}
	return c.JSON(base.APIResponse{
		Data: res,
		// The API does not report totals; hasMore is true while a page is full.
		Pagination: &base.PaginationControls{Page: uint(page), Limit: PageSize, HasMore: len(res) == PageSize},
		Meta:       fiber.Map{"source": "audio.com", "query": q, "cached": cached},
	})
}

var trackSorts = map[string]string{
	"recent":   "created_at DESC, id",
	"title":    "LOWER(title), id",
	"duration": "duration_ms DESC, id",
}

// Tracks lists stored audio.com tracks.
//
//	?q= ?tag= ?category= ?username= ?hasAudio= ?sort=recent|title|duration
func (s *Service) Tracks(c *fiber.Ctx) error {
	p := base.ParsePage(c)
	q := s.DB.Model(&Track{})
	if term := strings.TrimSpace(strings.ToLower(c.Query("q"))); term != "" {
		for _, w := range strings.Fields(term) {
			like := "%" + likeEscape(w) + "%"
			q = q.Where(`(LOWER(title) LIKE ? ESCAPE '\' OR LOWER(author_name) LIKE ? ESCAPE '\' OR LOWER(tags_index) LIKE ? ESCAPE '\')`, like, like, like)
		}
	}
	if t := strings.ToLower(strings.TrimPrefix(c.Query("tag"), "#")); t != "" {
		q = q.Where(`tags_index LIKE ? ESCAPE '\'`, "%,"+likeEscape(t)+",%")
	}
	if v := c.Query("category"); v != "" {
		q = q.Where("category = ?", v)
	}
	if v := c.Query("username"); v != "" {
		q = q.Where("username = ?", v)
	}
	if v := c.Query("hasAudio", "true"); v != "" {
		has, err := strconv.ParseBool(v)
		if err != nil {
			return base.API_ERROR(c, "hasAudio must be true or false")
		}
		if has {
			q = q.Where("play_url <> ''")
		} else {
			q = q.Where("play_url = ''")
		}
	}
	order, ok := trackSorts[c.Query("sort", "recent")]
	if !ok {
		return base.API_ERROR(c, "sort must be one of: recent, title, duration")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to count tracks")
	}
	tracks := []Track{}
	if err := p.Apply(q).Order(order).Find(&tracks).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get tracks")
	}
	return c.JSON(base.APIResponse{Data: tracks, Pagination: p.Controls(total, len(tracks))})
}

func (s *Service) Track(c *fiber.Ctx) error {
	var t Track
	if err := s.DB.Where("id = ?", c.Params("id")).Limit(1).Find(&t).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get track")
	}
	if t.ID == "" {
		return base.API_FAIL(c, fiber.StatusNotFound, "track not found")
	}
	return c.JSON(base.APIResponse{Data: t})
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
