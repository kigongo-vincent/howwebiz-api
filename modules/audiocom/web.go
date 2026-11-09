package audiocom

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
	"gorm.io/gorm"
)

// Website crawling of audio.com. Fetching goes through webguard (block
// counter + circuit breaker, robots.txt, no cookies, fixed User-Agent).
// Pages that load but contain no recognizable data are counted too
// (MaxUnrecognized), so a layout change stops the crawl instead of
// silently storing nothing.

const (
	KindAudiocomWeb = "audiocom-web"
	webHealthName   = "audio.com"
	webFeedPrefix   = "web:"
)

var (
	ErrCircuitOpen  = webguard.ErrCircuitOpen
	ErrBlocked      = webguard.ErrBlocked
	ErrDisallowed   = webguard.ErrDisallowed
	ErrUnrecognized = errors.New("page loaded but no recognizable track data (layout may have changed)")
)

type WebConfig struct {
	webguard.Config
	MaxUnrecognized int // data-less pages in a row before stopping
	Daily           bool
	GenreRefresh    time.Duration
}

func WebConfigFromEnv() WebConfig {
	return WebConfig{
		Config:          webguard.ConfigFromEnv("AUDIOCOM_WEB", "https://audio.com", 3*time.Second),
		MaxUnrecognized: max(1, base.EnvInt("AUDIOCOM_WEB_MAX_UNRECOGNIZED", 3)),
		Daily:           base.EnvBool("AUDIOCOM_WEB_DAILY", true),
		GenreRefresh:    base.EnvDuration("AUDIOCOM_WEB_GENRE_REFRESH", 7*24*time.Hour),
	}
}

// Web is audio.com's guarded fetcher plus the website-crawl settings.
type Web struct {
	*webguard.Fetcher
	Cfg WebConfig
}

func NewWeb(db *gorm.DB, cfg WebConfig) *Web {
	return &Web{Fetcher: webguard.New(db, webHealthName, cfg.Config), Cfg: cfg}
}

// ---- crawl ----

// WebCrawlOptions bound one website crawl.
type WebCrawlOptions struct {
	Genres   []string `json:"genres"`   // genre slugs; empty = every genre linked from /genres
	MaxPages int      `json:"maxPages"` // 0 = unlimited
	Reset    bool     `json:"reset"`
}

// StartWebCrawl walks /genres and each /genres/{slug}?page=N until a page
// adds nothing, storing tracks into the same table as the API source.
func (s *Service) StartWebCrawl(trigger string, opts WebCrawlOptions) (*crawler.CrawlRun, error) {
	if opts.MaxPages < 0 {
		return nil, errors.New("maxPages must be >= 0 (0 = unlimited)")
	}
	if open, until := s.Web.CircuitOpen(); open {
		return nil, fmt.Errorf("%w until %s; POST /api/v1/audiocom/web/reset to override", ErrCircuitOpen, until.Format(time.RFC3339))
	}
	return s.Runner.Launch(trigger, KindAudiocomWeb, opts, func(ctx context.Context, st *crawler.RunState) {
		s.Web.StartRun()
		if err := s.webCrawl(ctx, opts, st); err != nil && ctx.Err() == nil {
			st.Fail("%v", err)
		}
	})
}

func (s *Service) webCrawl(ctx context.Context, opts WebCrawlOptions, st *crawler.RunState) error {
	pages := s.Cfg.budget(opts.MaxPages)
	genres := opts.Genres
	if len(genres) == 0 {
		body, err := s.Web.Get(ctx, "/genres")
		if err != nil {
			return fmt.Errorf("genres index: %w", err)
		}
		pages.take(1)
		st.Add(func(x *crawler.Stats) { x.PagesFetched++ })
		pd := ExtractPage(body)
		if len(pd.Genres) == 0 {
			return fmt.Errorf("genres index: %w: no /genres/{name} links (%s)", ErrUnrecognized, pd.describe(len(body)))
		}
		genres = pd.Genres
	}
	unrecognized := 0
	for _, g := range genres {
		if ctx.Err() != nil || !pages.ok() {
			return nil
		}
		err := s.webGenre(ctx, g, opts.Reset, pages, &unrecognized, st)
		switch {
		case errors.Is(err, ErrBlocked), errors.Is(err, ErrCircuitOpen):
			return err // guard tripped: stop the whole run
		case errors.Is(err, ErrUnrecognized) && unrecognized >= s.Web.Cfg.MaxUnrecognized:
			return fmt.Errorf("stopping after %d pages without recognizable data: %w", unrecognized, err)
		case err != nil:
			st.Fail("genre %s: %v", g, err)
		}
	}
	return nil
}

func (s *Service) webGenre(ctx context.Context, genre string, reset bool, pages *budget, unrecognized *int, st *crawler.RunState) error {
	name := webFeedPrefix + "/genres/" + genre
	f := Feed{Name: name}
	if err := s.DB.Where("name = ?", name).Limit(1).Find(&f).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	switch {
	case reset || f.NextPage < 1:
		f = Feed{Name: name, NextPage: 1}
	case f.Complete && f.NextPage == 1 && f.WalkDoneAt != nil && now.Sub(*f.WalkDoneAt) < s.Web.Cfg.GenreRefresh:
		return nil
	}
	if f.NextPage == 1 {
		f.Tracks = 0
	}
	prev := ""
	for pages.ok() && ctx.Err() == nil {
		p := "/genres/" + url.PathEscape(genre)
		if f.NextPage > 1 {
			p += "?page=" + strconv.Itoa(f.NextPage) // the only query robots.txt allows here
		}
		body, err := s.Web.Get(ctx, p)
		if err != nil {
			return err
		}
		pages.take(1)
		st.Add(func(x *crawler.Stats) { x.PagesFetched++ })

		pd := ExtractPage(body)
		hash := scraper.Fingerprint(pd.Tracks)
		if len(pd.Tracks) == 0 && f.NextPage == 1 {
			*unrecognized++
			return fmt.Errorf("%w: %s %s", ErrUnrecognized, p, pd.describe(len(body)))
		}
		*unrecognized = 0
		if len(pd.Tracks) == 0 || hash == prev { // past the last page
			done := time.Now().UTC()
			f.Complete, f.NextPage, f.WalkDoneAt = true, 1, &done
			break
		}
		prev = hash
		us, err := Upsert(s.DB, pd.Tracks, time.Now().UTC())
		if err != nil {
			return err
		}
		st.Add(func(x *crawler.Stats) {
			x.SongsSeen += len(pd.Tracks)
			x.SongsInserted += us.Inserted
			x.SongsUpdated += us.Updated
			x.SongsUnchanged += us.Unchanged
		})
		f.Tracks += len(pd.Tracks)
		f.NextPage++
		if err := s.saveFeed(&f); err != nil {
			return err
		}
	}
	sync := time.Now().UTC()
	f.LastSyncAt = &sync
	return s.saveFeed(&f)
}

// WebSearch fetches audio.com/search?search_text=…&sort=relevance[&page=N].
func (s *Service) WebSearch(ctx context.Context, q string, page int) (PageData, error) {
	v := url.Values{"search_text": {q}, "sort": {"relevance"}}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	body, err := s.Web.Get(ctx, "/search?"+v.Encode())
	if err != nil {
		return PageData{}, err
	}
	pd := ExtractPage(body)
	if len(pd.Tracks) > 0 {
		if _, err := Upsert(s.DB, pd.Tracks, time.Now().UTC()); err != nil {
			s.log.Warn("audio.com web: store search results", "err", err)
		}
	}
	return pd, nil
}
