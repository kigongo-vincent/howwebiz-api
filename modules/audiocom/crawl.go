package audiocom

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm/clause"
)

const KindAudiocom = "audiocom"

// CrawlOptions bound one audio.com crawl run.
type CrawlOptions struct {
	// Tags to walk as "#tag" feeds (genre-like). Empty = AUDIOCOM_TAGS.
	Tags []string `json:"tags"`
	// SkipAll skips the browse-everything feed (tags only).
	SkipAll bool `json:"skipAll"`
	// MaxPages per run across all feeds; 0 = unlimited. 10 results/page,
	// 60 requests/minute → 600 tracks/minute at best.
	MaxPages int  `json:"maxPages"`
	Reset    bool `json:"reset"` // forget feed positions and walk again
}

func (o CrawlOptions) Validate() error {
	if o.MaxPages < 0 {
		return errors.New("maxPages must be >= 0 (0 = unlimited)")
	}
	for _, t := range o.Tags {
		if strings.TrimSpace(strings.TrimPrefix(t, "#")) == "" {
			return errors.New("tags must not be empty")
		}
	}
	return nil
}

func feedName(tag string) string {
	if tag == "" {
		return "all"
	}
	return "#" + strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tag), "#"))
}

// StartCrawl runs the audio.com crawl in the background.
//
//   - "all" (empty query, newest first): walked to its last page once,
//     resumably; after that each run reads from page 1 and stops at the
//     first page with no new or changed track.
//   - "#tag" feeds: walked fully (resumably), then re-walked every
//     AUDIOCOM_TAG_REFRESH — their order is relevance, not recency, so
//     there is no early stop. Unchanged tracks cost no writes.
func (s *Service) StartCrawl(trigger string, opts CrawlOptions) (*crawler.CrawlRun, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if !s.OAuth.Connected() {
		return nil, ErrNotConnected
	}
	if len(opts.Tags) == 0 {
		opts.Tags = s.Cfg.Tags
	}
	return s.Runner.Launch(trigger, KindAudiocom, opts, func(ctx context.Context, st *crawler.RunState) {
		pages := s.Cfg.budget(opts.MaxPages)
		feeds := []string{}
		if !opts.SkipAll {
			feeds = append(feeds, "")
		}
		feeds = append(feeds, opts.Tags...)
		for _, tag := range feeds {
			if ctx.Err() != nil || !pages.ok() {
				return
			}
			if err := s.crawlFeed(ctx, tag, opts.Reset, pages, st); err != nil {
				if ctx.Err() != nil {
					return
				}
				st.Fail("%s: %v", feedName(tag), err)
				if errors.Is(err, ErrNotConnected) {
					return
				}
			}
		}
	})
}

func (s *Service) crawlFeed(ctx context.Context, tag string, reset bool, pages *budget, st *crawler.RunState) error {
	name := feedName(tag)
	query := ""
	if tag != "" {
		query = name // "#tag"
	}
	f := Feed{Name: name}
	if err := s.DB.Where("name = ?", name).Limit(1).Find(&f).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	incremental := false
	switch {
	case reset || f.NextPage < 1:
		f = Feed{Name: name, NextPage: 1}
	case f.Complete && tag == "":
		incremental = true // newest-first: stop at the first page with nothing new
	case f.Complete && f.NextPage == 1 && f.WalkDoneAt != nil && now.Sub(*f.WalkDoneAt) < s.Cfg.TagRefresh:
		return nil // tag feed walked recently
	}
	if f.NextPage == 1 {
		f.Tracks = 0
	}

	prev := ""
	for pages.ok() {
		if ctx.Err() != nil {
			return nil
		}
		res, err := s.Client.Search(ctx, query, "audio", f.NextPage)
		if err != nil {
			return fmt.Errorf("page %d: %w", f.NextPage, err)
		}
		pages.take(1)
		st.Add(func(x *crawler.Stats) { x.PagesFetched++ })

		audios := audioResults(res)
		hash := scraper.Fingerprint(audios)
		if len(audios) == 0 || hash == prev { // past the last page
			done := time.Now().UTC()
			f.Complete, f.NextPage, f.WalkDoneAt = true, 1, &done
			break
		}
		prev = hash
		us, err := Upsert(s.DB, audios, time.Now().UTC())
		if err != nil {
			return err
		}
		st.Add(func(x *crawler.Stats) {
			x.SongsSeen += len(audios)
			x.SongsInserted += us.Inserted
			x.SongsUpdated += us.Updated
			x.SongsUnchanged += us.Unchanged
		})
		f.Tracks += len(audios)
		f.NextPage++
		if incremental && us.Inserted == 0 && us.Updated == 0 {
			f.NextPage = 1
			break
		}
		if err := s.saveFeed(&f); err != nil {
			return err
		}
		if f.NextPage%100 == 0 {
			s.log.Info("audio.com: crawl progress", "feed", name, "page", f.NextPage, "tracks", f.Tracks)
		}
	}
	sync := time.Now().UTC()
	f.LastSyncAt = &sync
	return s.saveFeed(&f)
}

func (s *Service) saveFeed(f *Feed) error {
	f.UpdatedAt = time.Now().UTC()
	return s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(f).Error
}

type budget struct {
	left      int
	unlimited bool
}

func (c Config) budget(n int) *budget { return &budget{left: n, unlimited: n == 0} }
func (b *budget) ok() bool            { return b.unlimited || b.left > 0 }
func (b *budget) take(n int) {
	if !b.unlimited {
		b.left -= n
	}
}
