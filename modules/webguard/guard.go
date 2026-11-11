// Package webguard fetches web pages politely and stops when a site blocks
// it. It never tries to get around a block.
//
// Requests are plain and honest: one fixed User-Agent, no cookie jar (so a
// challenge clearance is never carried), robots.txt obeyed, one request at
// a time with a delay. A block (Cloudflare challenge, 403/429/503) is
// counted, not evaded:
//
//   - each block → exponential backoff, then the same URL with the same
//     headers is retried
//   - MaxConsecutive blocks in a row, or MaxPerRun in one run → the circuit
//     opens: every request for that site is refused for Cooldown (doubling
//     each time it re-opens, up to MaxCooldown) or until an operator resets it
//   - a page that loads resets the consecutive count
package webguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrCircuitOpen = errors.New("web crawling is paused after repeated blocks")
	ErrBlocked     = errors.New("blocked by the site")
	ErrDisallowed  = errors.New("disallowed by robots.txt")
	ErrNotFound    = errors.New("not found")
)

type Config struct {
	BaseURL        string
	UserAgent      string
	Delay          time.Duration // between requests
	MaxConsecutive int           // blocks in a row before the circuit opens
	MaxPerRun      int           // blocks in one run before the circuit opens
	Backoff        time.Duration // first backoff after a block; doubles
	Cooldown       time.Duration // first circuit-open pause; doubles per trip
	MaxCooldown    time.Duration
}

// ConfigFromEnv reads <PREFIX>_* variables (e.g. AUDIOCOM_WEB_DELAY).
func ConfigFromEnv(prefix, baseURL string, delay time.Duration) Config {
	return Config{
		BaseURL:        strings.TrimRight(base.Env(prefix+"_BASE_URL", baseURL), "/"),
		UserAgent:      base.Env(prefix+"_USER_AGENT", base.Env("CRAWL_USER_AGENT", "howwebiz-api-crawler/1.0")),
		Delay:          base.EnvDuration(prefix+"_DELAY", delay),
		MaxConsecutive: max(1, base.EnvInt(prefix+"_MAX_CONSECUTIVE_BLOCKS", 3)),
		MaxPerRun:      max(1, base.EnvInt(prefix+"_MAX_BLOCKS_PER_RUN", 5)),
		Backoff:        base.EnvDuration(prefix+"_BLOCK_BACKOFF", 30*time.Second),
		Cooldown:       base.EnvDuration(prefix+"_BLOCK_COOLDOWN", 24*time.Hour),
		MaxCooldown:    base.EnvDuration(prefix+"_MAX_COOLDOWN", 7*24*time.Hour),
	}
}

// Health is the persisted block counter / circuit breaker state, one row per site.
type Health struct {
	Name              string     `json:"-" gorm:"primaryKey;size:32"`
	ConsecutiveBlocks int        `json:"consecutiveBlocks"`
	TotalBlocks       int        `json:"totalBlocks"`
	TotalRequests     int        `json:"totalRequests"`
	TotalOK           int        `json:"totalOk"`
	Trips             int        `json:"trips"` // times the circuit opened (drives cooldown growth)
	BlockedUntil      *time.Time `json:"blockedUntil"`
	LastBlockAt       *time.Time `json:"lastBlockAt"`
	LastBlockStatus   int        `json:"lastBlockStatus"`
	LastBlockReason   string     `json:"lastBlockReason"`
	LastBlockURL      string     `json:"lastBlockUrl"`
	LastOKAt          *time.Time `json:"lastOkAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func (Health) TableName() string { return "web_health" }

// Fetcher fetches one site's pages behind the guard.
type Fetcher struct {
	DB   *gorm.DB
	Name string // site key in web_health, e.g. "audio.com"
	Cfg  Config
	http *http.Client

	mu        sync.Mutex // serializes requests and guard updates
	next      time.Time
	robots    *scraper.Robots
	robotsAt  time.Time
	runBlocks int // blocks in the current run (reset by StartRun)
}

func New(db *gorm.DB, name string, cfg Config) *Fetcher {
	return &Fetcher{DB: db, Name: name, Cfg: cfg, http: &http.Client{
		Timeout: 30 * time.Second,
		// Follow redirects only within the site; no cookie jar on purpose.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}
}

func (w *Fetcher) Health() (Health, error) {
	h := Health{Name: w.Name}
	err := w.DB.Where("name = ?", w.Name).Limit(1).Find(&h).Error
	return h, err
}

func (w *Fetcher) saveHealth(h *Health) error {
	h.Name, h.UpdatedAt = w.Name, time.Now().UTC()
	return w.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(h).Error
}

// ResetCircuit clears the pause and the consecutive counter (operator action).
func (w *Fetcher) ResetCircuit() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	h, err := w.Health()
	if err != nil {
		return err
	}
	h.ConsecutiveBlocks, h.BlockedUntil = 0, nil
	return w.saveHealth(&h)
}

// StartRun resets the per-run block budget.
func (w *Fetcher) StartRun() {
	w.mu.Lock()
	w.runBlocks = 0
	w.mu.Unlock()
}

// CircuitOpen reports whether web crawling is paused, and until when.
func (w *Fetcher) CircuitOpen() (bool, *time.Time) {
	h, err := w.Health()
	if err != nil || h.BlockedUntil == nil || time.Now().After(*h.BlockedUntil) {
		return false, nil
	}
	return true, h.BlockedUntil
}

// blockReason classifies a response as a block. It never inspects or
// reuses challenge tokens; it only recognizes them.
func blockReason(resp *http.Response, body []byte) string {
	if resp.Header.Get("cf-mitigated") != "" {
		return "cloudflare challenge (cf-mitigated: " + resp.Header.Get("cf-mitigated") + ")"
	}
	b := string(body[:min(len(body), 4096)])
	switch {
	case strings.Contains(b, "<title>Just a moment...</title>"), strings.Contains(b, "challenge-platform"),
		strings.Contains(b, "cf-chl-"), strings.Contains(b, "Attention Required! | Cloudflare"):
		return "challenge page"
	case resp.StatusCode == http.StatusTooManyRequests:
		return "rate limited (429)"
	case resp.StatusCode == http.StatusForbidden:
		return "forbidden (403)"
	case resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare"):
		return "cloudflare 503"
	}
	return ""
}

func (w *Fetcher) allowed(ctx context.Context, u string) (bool, error) {
	if w.robots == nil || time.Since(w.robotsAt) > 6*time.Hour {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.Cfg.BaseURL+"/robots.txt", nil)
		req.Header.Set("User-Agent", w.Cfg.UserAgent)
		resp, err := w.http.Do(req)
		if err != nil {
			return false, fmt.Errorf("robots.txt: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK && blockReason(resp, body) == "":
			w.robots = scraper.ParseRobots(string(body), w.Cfg.UserAgent)
		case resp.StatusCode >= 400 && resp.StatusCode < 500 && blockReason(resp, body) == "":
			w.robots = scraper.ParseRobots("", w.Cfg.UserAgent) // no robots.txt = allowed
		default:
			return false, fmt.Errorf("robots.txt unavailable (%d); not crawling without it", resp.StatusCode)
		}
		w.robotsAt = time.Now()
	}
	return w.robots.Allowed(u), nil
}

// Get fetches path (with query) behind every guard. On a block it records
// it, backs off, and retries the same URL until the guard opens the
// circuit — then it returns ErrBlocked and the caller must stop.
func (w *Fetcher) Get(ctx context.Context, pathAndQuery string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	u := w.Cfg.BaseURL + pathAndQuery

	for {
		h, err := w.Health()
		if err != nil {
			return nil, err
		}
		if h.BlockedUntil != nil && time.Now().Before(*h.BlockedUntil) {
			return nil, fmt.Errorf("%w until %s (last: %s on %s)", ErrCircuitOpen,
				h.BlockedUntil.Format(time.RFC3339), h.LastBlockReason, h.LastBlockURL)
		}
		ok, err := w.allowed(ctx, u)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrDisallowed, pathAndQuery)
		}
		if err := sleepCtx(ctx, time.Until(w.next)); err != nil {
			return nil, err
		}
		// The site's robots.txt Crawl-delay wins when it asks for more.
		w.next = time.Now().Add(max(w.Cfg.Delay, w.robots.CrawlDelay))

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("User-Agent", w.Cfg.UserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, err := w.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", pathAndQuery, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", pathAndQuery, err)
		}
		h.TotalRequests++
		now := time.Now().UTC()

		if reason := blockReason(resp, body); reason != "" {
			w.runBlocks++
			h.ConsecutiveBlocks++
			h.TotalBlocks++
			h.LastBlockAt, h.LastBlockStatus, h.LastBlockReason, h.LastBlockURL = &now, resp.StatusCode, reason, pathAndQuery
			if h.ConsecutiveBlocks >= w.Cfg.MaxConsecutive || w.runBlocks >= w.Cfg.MaxPerRun {
				h.Trips++
				cool := min(w.Cfg.Cooldown<<min(h.Trips-1, 10), w.Cfg.MaxCooldown)
				until := now.Add(cool)
				h.BlockedUntil = &until
				if err := w.saveHealth(&h); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("%w (%s): %s on %s (%d in a row, %d this run); web crawling paused until %s",
					ErrBlocked, w.Name, reason, pathAndQuery, h.ConsecutiveBlocks, w.runBlocks, until.Format(time.RFC3339))
			}
			if err := w.saveHealth(&h); err != nil {
				return nil, err
			}
			wait := w.Cfg.Backoff << min(h.ConsecutiveBlocks-1, 10)
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && time.Duration(s)*time.Second > wait {
				wait = time.Duration(s) * time.Second
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue // same URL, same headers: no evasion, just patience
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			h.ConsecutiveBlocks, h.TotalOK, h.LastOKAt = 0, h.TotalOK+1, &now
			return body, w.saveHealth(&h)
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone: // 410: removed for good
			_ = w.saveHealth(&h)
			return nil, fmt.Errorf("GET %s: %w", pathAndQuery, ErrNotFound)
		default:
			_ = w.saveHealth(&h)
			return nil, fmt.Errorf("GET %s: status %d", pathAndQuery, resp.StatusCode)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
