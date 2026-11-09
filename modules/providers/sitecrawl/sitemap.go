package sitecrawl

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"gorm.io/gorm/clause"
)

// Page is a song page known from a sitemap.
type Page struct {
	Site      string     `gorm:"primaryKey;size:32"`
	Path      string     `gorm:"primaryKey;size:512"`
	LastMod   *time.Time `gorm:"index"`
	FetchedAt *time.Time `gorm:"index"`
	// FetchedMod is the lastmod the page had when we fetched it. A page is
	// due when the sitemap's lastmod differs from it — never compare
	// lastmod with our own clock (sites stamp day-granular or skewed dates).
	FetchedMod *time.Time
	Missing    bool // the page had no song last time
}

func (Page) TableName() string { return "sitecrawl_pages" }

type sitemapXML struct {
	URLs     []entry `xml:"url"`
	Sitemaps []entry `xml:"sitemap"`
}

type entry struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

func parseTime(s string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", time.DateOnly} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

const dueSQL = "(fetched_at IS NULL OR (last_mod IS NOT NULL AND (fetched_mod IS NULL OR last_mod <> fetched_mod)))"

// sitemapPass refreshes the page list from the sitemaps, then fetches
// pages never fetched or modified since (most recent first) within budget.
func (c *Crawler) sitemapPass(ctx context.Context, sm Sitemapper, budget *budget, st *crawler.RunState) error {
	found := map[string]*time.Time{}
	queue := append([]string{}, sm.SitemapPaths()...)
	for depth := 0; len(queue) > 0 && depth < 1000 && ctx.Err() == nil; depth++ {
		p := queue[0]
		queue = queue[1:]
		body, err := c.Web.Get(ctx, p)
		if fatal(err) {
			return fmt.Errorf("sitemap %s: %w", p, err)
		}
		if err != nil { // one broken sitemap must not stop the others
			st.Fail("sitemap %s: %v", p, err)
			continue
		}
		st.Add(func(x *crawler.Stats) { x.PagesFetched++ })
		var doc sitemapXML
		if err := xml.Unmarshal(body, &doc); err != nil {
			st.Fail("sitemap %s: %v", p, err)
			continue
		}
		for _, s := range doc.Sitemaps { // sitemapindex → follow children
			if u, err := url.Parse(s.Loc); err == nil {
				queue = append(queue, u.RequestURI())
			}
		}
		for _, e := range doc.URLs {
			u, err := url.Parse(e.Loc)
			if err != nil || !sm.IsSongPage(u.Path) {
				continue
			}
			found[u.RequestURI()] = parseTime(e.LastMod)
		}
	}

	// Store lastmod changes only (most of a big sitemap is unchanged).
	var known []Page
	if err := c.DB.Where("site = ?", c.Site.Name()).Find(&known).Error; err != nil {
		return err
	}
	have := make(map[string]Page, len(known))
	for _, p := range known {
		have[p.Path] = p
	}
	var upserts []Page
	for path, lm := range found {
		old, ok := have[path]
		if ok && ((old.LastMod == nil && lm == nil) || (old.LastMod != nil && lm != nil && old.LastMod.Equal(*lm))) {
			continue
		}
		upserts = append(upserts, Page{Site: c.Site.Name(), Path: path, LastMod: lm, FetchedAt: old.FetchedAt,
			FetchedMod: old.FetchedMod, Missing: old.Missing})
	}
	if len(upserts) > 0 {
		if err := c.DB.Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(upserts, 500).Error; err != nil {
			return err
		}
	}

	// Due: never fetched, or modified after the last fetch.
	q := c.DB.Where("site = ? AND "+dueSQL, c.Site.Name()).
		Order("last_mod DESC")
	if !budget.unlimited {
		q = q.Limit(max(budget.left, 0))
	}
	var due []Page
	if err := q.Find(&due).Error; err != nil {
		return err
	}
	for _, p := range due {
		if !budget.ok() || ctx.Err() != nil {
			return nil
		}
		body, err := c.Web.Get(ctx, p.Path)
		if fatal(err) {
			return err
		}
		budget.take()
		now := time.Now().UTC()
		p.FetchedAt, p.FetchedMod = &now, p.LastMod
		if err != nil {
			st.Fail("page %s: %v", p.Path, err)
			p.Missing = true
			c.DB.Save(&p)
			continue
		}
		st.Add(func(x *crawler.Stats) { x.DetailsFetched++ })
		tracks, err := sm.ParseSongPage(body)
		if errors.Is(err, ErrNotFound) || (err == nil && len(tracks) == 0) {
			p.Missing = true
			c.DB.Save(&p)
			continue
		}
		if err != nil {
			st.Fail("page %s: %v", p.Path, err)
			continue
		}
		is, err := c.Ingester.Ingest(tracks, now)
		if err != nil {
			return err
		}
		st.Add(func(x *crawler.Stats) {
			x.SongsInserted += is.Inserted
			x.SongsUpdated += is.Updated
			x.SongsUnchanged += is.Unchanged
			if is.Updated > 0 {
				x.DetailsChanged++
			} else {
				x.DetailsUnchanged++
			}
		})
		p.Missing = false
		if err := c.DB.Save(&p).Error; err != nil {
			return err
		}
	}
	return nil
}

// sitemapCounts reports page totals for the status endpoint.
func (c *Crawler) sitemapCounts() map[string]int64 {
	var total, fetched, due int64
	c.DB.Model(&Page{}).Where("site = ?", c.Site.Name()).Count(&total)
	c.DB.Model(&Page{}).Where("site = ? AND fetched_at IS NOT NULL", c.Site.Name()).Count(&fetched)
	c.DB.Model(&Page{}).Where("site = ? AND "+dueSQL, c.Site.Name()).Count(&due)
	return map[string]int64{"pages": total, "fetched": fetched, "due": due}
}
