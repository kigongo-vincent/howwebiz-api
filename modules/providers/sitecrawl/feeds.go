package sitecrawl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm/clause"
)

// Fetch gets a path (with query) on the provider's BaseURL through the
// guard (robots.txt, delay, block counter). Providers that read an API
// use it instead of building their own HTTP client.
type Fetch func(ctx context.Context, pathAndQuery string) ([]byte, error)

// Feeder: an API (or site) with one or more feeds of full tracks, paged by
// an opaque cursor. A feed is e.g. a genre or a collection.
//
//   - Head feeds are newest first: walked once to their end (resumable),
//     then each run reads from the start until a page adds nothing new.
//   - Tail feeds are oldest first (ascending by a watermark the cursor
//     encodes): every run resumes from the stored cursor, so new items
//     are picked up at the tail without re-reading the feed.
type Feeder interface {
	Feeds() []string
	// FeedPage reads one page of feed at cursor ("" = start). done=true at
	// the end of the feed; a tail feed then returns, in next, the cursor
	// to resume from later.
	FeedPage(ctx context.Context, get Fetch, feed, cursor string) (tracks []catalog.Track, next string, done bool, err error)
	// Tail reports whether feed is oldest first (see above).
	Tail(feed string) bool
}

// APISearcher: live search through an API (instead of an HTML search page).
type APISearcher interface {
	SearchAPI(ctx context.Context, get Fetch, q string, limit int) ([]catalog.Track, error)
}

// Enabler: a provider that needs configuration (e.g. an API key). When it
// reports false the provider is listed but never crawled or searched.
type Enabler interface {
	Enabled() bool
}

// ErrNotConfigured: the provider needs configuration before it can crawl.
var ErrNotConfigured = errors.New("provider is not configured")

// FeedCursor is one feed's resumable position.
type FeedCursor struct {
	Site       string     `json:"-" gorm:"primaryKey;size:32"`
	Feed       string     `json:"feed" gorm:"primaryKey;size:128"`
	Cursor     string     `json:"cursor" gorm:"size:2048"`
	WalkDoneAt *time.Time `json:"walkDoneAt"` // first full walk finished
	Pages      int64      `json:"pages"`      // pages read so far
	Tracks     int64      `json:"tracks"`     // tracks seen so far
	LastSyncAt *time.Time `json:"lastSyncAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

func (FeedCursor) TableName() string { return "sitecrawl_feeds" }

func (c *Crawler) enabled() bool {
	e, ok := c.Site.(Enabler)
	return !ok || e.Enabled()
}

func (c *Crawler) feedCursors() map[string]FeedCursor {
	var rows []FeedCursor
	c.DB.Where("site = ?", c.Site.Name()).Find(&rows)
	out := make(map[string]FeedCursor, len(rows))
	for _, r := range rows {
		out[r.Feed] = r
	}
	return out
}

func (c *Crawler) saveFeed(fc *FeedCursor) error {
	fc.UpdatedAt = time.Now().UTC()
	return c.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(fc).Error
}

// feedPass advances every feed within the page budget. Feeds take turns
// one page at a time, so a small budget still reaches every feed.
func (c *Crawler) feedPass(ctx context.Context, f Feeder, reset bool, pages *budget, st *crawler.RunState) error {
	if reset {
		if err := c.DB.Where("site = ?", c.Site.Name()).Delete(&FeedCursor{}).Error; err != nil {
			return err
		}
	}
	type state struct {
		fc       FeedCursor
		head     bool   // incremental read of a synced head feed
		pos      string // position of that read (not persisted)
		prev     string // fingerprint of the previous page (repeat = end)
		finished bool
	}
	stored := c.feedCursors()
	var feeds []*state
	for _, name := range f.Feeds() {
		fc, ok := stored[name]
		if !ok {
			fc = FeedCursor{Site: c.Site.Name(), Feed: name}
		}
		feeds = append(feeds, &state{fc: fc, head: fc.WalkDoneAt != nil && !f.Tail(name)})
	}
	get := Fetch(c.Web.Get)
	for active := len(feeds); active > 0 && pages.ok() && ctx.Err() == nil; {
		active = 0
		for _, s := range feeds {
			if s.finished || !pages.ok() || ctx.Err() != nil {
				continue
			}
			active++
			cursor := s.fc.Cursor
			if s.head {
				cursor = s.pos
			}
			tracks, next, done, err := f.FeedPage(ctx, get, s.fc.Feed, cursor)
			if err != nil {
				if fatal(err) || ctx.Err() != nil {
					return err
				}
				st.Fail("%s feed %q: %v", c.Site.Name(), s.fc.Feed, err)
				s.finished = true
				continue
			}
			pages.take()
			st.Add(func(x *crawler.Stats) { x.PagesFetched++; x.SongsSeen += len(tracks) })
			hash := scraper.Fingerprint(tracks)
			if len(tracks) > 0 && hash == s.prev { // a feed repeating its last page
				done, next = true, cursor
			}
			s.prev = hash
			now := time.Now().UTC()
			is, err := c.Ingester.Ingest(tracks, now)
			if err != nil {
				return err
			}
			st.Add(func(x *crawler.Stats) {
				x.SongsInserted += is.Inserted
				x.SongsUpdated += is.Updated
				x.SongsUnchanged += is.Unchanged
			})
			s.fc.Pages++
			s.fc.Tracks += int64(len(tracks))
			s.fc.LastSyncAt = &now
			switch {
			case s.head:
				// Newest first: stop at the first page with nothing new.
				s.pos = next
				s.finished = done || is.Inserted == 0
			case done:
				if s.fc.WalkDoneAt == nil {
					s.fc.WalkDoneAt = &now
				}
				s.fc.Cursor = ""
				if f.Tail(s.fc.Feed) {
					s.fc.Cursor = next
				}
				s.finished = true
			default:
				s.fc.Cursor = next
			}
			if err := c.saveFeed(&s.fc); err != nil {
				return err
			}
		}
	}
	return nil
}

// feedsSynced: every feed has been walked once.
func (c *Crawler) feedsSynced(f Feeder) bool {
	stored := c.feedCursors()
	for _, name := range f.Feeds() {
		if stored[name].WalkDoneAt == nil {
			return false
		}
	}
	return true
}

func notConfigured(name string) error { return fmt.Errorf("%w: %s", ErrNotConfigured, name) }
